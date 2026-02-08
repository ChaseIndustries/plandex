package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FixBuildPayload matches the JSON sent by Crewboard (lib/plandex-build-fix.ts).
type FixBuildPayload struct {
	Repo              FixBuildRepo   `json:"repo"`
	HeadBranch        string         `json:"headBranch"`
	HeadSha           string         `json:"headSha"`
	Annotations       []FixBuildAnno `json:"annotations"`
	OutputSummary     string         `json:"outputSummary"`
	InstallationToken string         `json:"installationToken"`
	CheckRunUrl       string         `json:"checkRunUrl,omitempty"`
	WorkflowRunUrl    string         `json:"workflowRunUrl,omitempty"`
}

type FixBuildRepo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type FixBuildAnno struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"`
	Message         string `json:"message"`
	Title           string `json:"title,omitempty"`
	RawDetails      string `json:"raw_details,omitempty"`
}

type FixBuildResponse struct {
	Ok        bool   `json:"ok"`
	CommitSha string `json:"commitSha,omitempty"`
}

const fixBuildTimeout = 15 * time.Minute

// FixBuildHandler handles POST /fix_build from Crewboard. Clones the repo at the failing
// commit, runs plandex to fix the failing test, commits and pushes (no new branch/PR).
func FixBuildHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[fix_build] read body: %v", err)
		http.Error(w, "error reading request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var payload FixBuildPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("[fix_build] parse body: %v", err)
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if payload.Repo.Owner == "" || payload.Repo.Name == "" || payload.HeadBranch == "" || payload.HeadSha == "" || payload.InstallationToken == "" {
		http.Error(w, "missing required fields: repo.owner, repo.name, headBranch, headSha, installationToken", http.StatusBadRequest)
		return
	}

	workDir, err := os.MkdirTemp("", "plandex-fix-build-*")
	if err != nil {
		log.Printf("[fix_build] mkdir temp: %v", err)
		http.Error(w, "failed to create work dir", http.StatusInternalServerError)
		return
	}
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			log.Printf("[fix_build] cleanup work dir: %v", err)
		}
	}()

	cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		payload.InstallationToken, payload.Repo.Owner, payload.Repo.Name)

	// Clone
	if out, err := runCmd(workDir, fixBuildTimeout, "git", "clone", "--depth", "50", cloneURL, "."); err != nil {
		log.Printf("[fix_build] clone: %v\n%s", err, out)
		http.Error(w, "clone failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Checkout branch and reset to failing SHA
	if out, err := runCmd(workDir, 30*time.Second, "git", "checkout", payload.HeadBranch); err != nil {
		log.Printf("[fix_build] checkout branch: %v\n%s", err, out)
		http.Error(w, "checkout branch failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if out, err := runCmd(workDir, 30*time.Second, "git", "reset", "--hard", payload.HeadSha); err != nil {
		log.Printf("[fix_build] reset to sha: %v\n%s", err, out)
		http.Error(w, "reset failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Write context file for plandex
	ctxPath := filepath.Join(workDir, "BUILD_FAILURE_CONTEXT.md")
	ctxContent := buildContextContent(payload)
	if err := os.WriteFile(ctxPath, []byte(ctxContent), 0644); err != nil {
		log.Printf("[fix_build] write context: %v", err)
		http.Error(w, "failed to write context file", http.StatusInternalServerError)
		return
	}

	prompt := "Fix the failing test(s) or build. Read BUILD_FAILURE_CONTEXT.md for the failure output and annotations. Apply minimal changes, then run the failing test or build command to verify it passes. Do not create a new branch or open a PR."

	// Run plandex tell (non-interactive)
	if _, err := exec.LookPath("plandex"); err != nil {
		log.Printf("[fix_build] plandex not in PATH: %v", err)
		http.Error(w, "plandex CLI not available in PATH; add plandex to the server image for fix_build", http.StatusNotImplemented)
		return
	}

	if out, err := runCmd(workDir, fixBuildTimeout, "plandex", "tell", prompt, "--skip-menu"); err != nil {
		log.Printf("[fix_build] plandex tell: %v\n%s", err, out)
		http.Error(w, "plandex tell failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Run plandex build to apply and verify
	if out, err := runCmd(workDir, fixBuildTimeout, "plandex", "build", "--skip-menu"); err != nil {
		log.Printf("[fix_build] plandex build: %v\n%s", err, out)
		http.Error(w, "plandex build failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Commit
	commitMsg := "fix: resolve failing test from CI"
	if out, err := runCmd(workDir, 30*time.Second, "git", "add", "-A"); err != nil {
		log.Printf("[fix_build] git add: %v\n%s", err, out)
		http.Error(w, "git add failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if out, err := runCmd(workDir, 30*time.Second, "git", "commit", "-m", commitMsg); err != nil {
		// Nothing to commit is possible if plandex made no changes
		if !strings.Contains(string(out), "nothing to commit") {
			log.Printf("[fix_build] git commit: %v\n%s", err, out)
			http.Error(w, "git commit failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Get commit SHA for response (if we committed)
	var commitSha string
	if out, err := runCmd(workDir, 10*time.Second, "git", "rev-parse", "HEAD"); err == nil {
		commitSha = strings.TrimSpace(string(out))
	}

	// Push using token in remote URL
	if out, err := runCmd(workDir, 60*time.Second, "git", "push", "origin", payload.HeadBranch); err != nil {
		log.Printf("[fix_build] git push: %v\n%s", err, out)
		http.Error(w, "git push failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(FixBuildResponse{Ok: true, CommitSha: commitSha})
}

func buildContextContent(p FixBuildPayload) string {
	var b strings.Builder
	b.WriteString("# Build failure context\n\n")
	if p.OutputSummary != "" {
		b.WriteString("## Output summary\n\n")
		b.WriteString(p.OutputSummary)
		b.WriteString("\n\n")
	}
	if p.CheckRunUrl != "" {
		b.WriteString("Check run: ")
		b.WriteString(p.CheckRunUrl)
		b.WriteString("\n\n")
	}
	if p.WorkflowRunUrl != "" {
		b.WriteString("Workflow run: ")
		b.WriteString(p.WorkflowRunUrl)
		b.WriteString("\n\n")
	}
	if len(p.Annotations) > 0 {
		b.WriteString("## Annotations\n\n")
		for _, a := range p.Annotations {
			b.WriteString(fmt.Sprintf("- **%s** (lines %d-%d): %s\n", a.Path, a.StartLine, a.EndLine, a.Message))
			if a.Title != "" {
				b.WriteString(fmt.Sprintf("  - %s\n", a.Title))
			}
			if a.RawDetails != "" {
				b.WriteString("  - Details:\n")
				for _, line := range strings.Split(a.RawDetails, "\n") {
					b.WriteString("    ")
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
	}
	return b.String()
}

func runCmd(dir string, timeout time.Duration, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		return out, err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return out, fmt.Errorf("command timed out after %v", timeout)
	}
}


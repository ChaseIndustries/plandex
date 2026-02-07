FROM golang:1.23.3

# Install build tools and Python for LiteLLM
RUN apt-get update && \
  apt-get install -y git gcc g++ make python3 python3-venv && \
  rm -rf /var/lib/apt/lists/*

# Create and activate a virtualenv for LiteLLM deps
RUN python3 -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

# Install LiteLLM passthrough dependencies
RUN pip install --no-cache-dir \
  "litellm==1.72.6" \
  "fastapi==0.115.12" \
  "uvicorn==0.34.1" \
  "google-cloud-aiplatform==1.96.0" \
  "boto3==1.38.40" \
  "botocore==1.38.40"

WORKDIR /app

# Download Go dependencies first for better caching
COPY ./app/shared/go.mod ./app/shared/go.sum ./shared/
RUN cd shared && go mod download

COPY ./app/server/go.mod ./app/server/go.sum ./server/
RUN cd server && go mod download

# Copy source
COPY ./app/server ./server
COPY ./app/shared ./shared
COPY ./app/scripts /scripts

# Build the server
WORKDIR /app/server
RUN rm -f plandex-server && go build -o plandex-server .

# Default runtime config
ENV PORT=8099
EXPOSE 8099

CMD ["./plandex-server"]
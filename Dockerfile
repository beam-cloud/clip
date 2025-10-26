FROM golang:latest

WORKDIR /workspace

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    fuse3 libfuse3-dev && \
    rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the library - no CLI needed, only programmatic API
RUN go build -v ./pkg/...

RUN mkdir -p /tmp/test

CMD ["tail", "-f", "/dev/null"]

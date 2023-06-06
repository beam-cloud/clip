# Start from the latest golang base image
FROM golang:latest

# Set the Current Working Directory inside the container
WORKDIR /workspace

# Install necessary packages for FUSE
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    fuse3 libfuse3-dev && \
    rm -rf /var/lib/apt/lists/*

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies
RUN go mod download

COPY . .

RUN go build -o /workspace/bin/cfs /workspace/cmd/fs/main.go

RUN mkdir -p /tmp/test

CMD ["tail", "-f", "/dev/null"]

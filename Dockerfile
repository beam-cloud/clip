FROM golang:latest

WORKDIR /workspace

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    fuse3 libfuse3-dev && \
    rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o /workspace/bin/clip /workspace/cmd/fs/main.go

RUN mkdir -p /tmp/test

CMD ["tail", "-f", "/dev/null"]

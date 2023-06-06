# Start from the latest golang base image
FROM golang:latest

# Set the Current Working Directory inside the container
WORKDIR /app

# Install necessary packages for FUSE
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    fuse3 libfuse3-dev && \
    rm -rf /var/lib/apt/lists/*

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container
COPY . .

# Build the application
RUN go build -o main .

RUN mkdir -p /tmp/test

# Command to run the application
CMD ["./main"]

#!/bin/bash

# Get OS and architecture details
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

# Map to values used in your GitHub releases
if [ "$ARCH" = "x86_64" ]; then
    ARCH="amd64"
elif [ "$ARCH" = "arm64" ]; then
    ARCH="arm64"
elif [ "$ARCH" = "aarch64" ]; then
    ARCH="arm64"
else
    echo "Unsupported architecture: $ARCH"
    exit 1
fi


# Get the download URL for the latest release
DOWNLOAD_URL=$(curl -s https://api.github.com/repos/beam-cloud/clip/releases/latest \
| grep browser_download_url \
| grep $OS-$ARCH \
| cut -d '"' -f 4)

# Check if the download URL is empty
if [ -z "$DOWNLOAD_URL" ]; then
    echo "Could not find a release for your OS and architecture ($OS-$ARCH)"
    exit 1
fi

# Download the binary
curl -Lo clip $DOWNLOAD_URL

# Make the binary executable
chmod +x clip

# Move the binary to /usr/local/bin
sudo mv clip /usr/local/bin

echo "Installation completed"

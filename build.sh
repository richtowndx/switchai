#!/bin/bash
# SwitchAI Build Script
# This script builds the SwitchAI binary for Windows and Linux with embedded web assets

set -e

echo "========================================"
echo "SwitchAI Build Script"
echo "========================================"
echo ""

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "ERROR: Go is not installed or not in PATH"
    exit 1
fi

# Get git commit hash and version info
GIT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "")
GIT_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "")

# Parse version from tag (format: v1.2.3)
VERSION_MAJOR=0
VERSION_MINOR=0
VERSION_PATCH=0
if [ -n "$GIT_TAG" ]; then
    VERSION_MAJOR=$(echo "$GIT_TAG" | cut -d. -f1 | sed 's/v//')
    VERSION_MINOR=$(echo "$GIT_TAG" | cut -d. -f2)
    VERSION_PATCH=$(echo "$GIT_TAG" | cut -d. -f3)
fi

echo "Version: v${VERSION_MAJOR}.${VERSION_MINOR}.${VERSION_PATCH}"
echo "Commit: ${GIT_COMMIT}"
echo ""

# Build ldflags
LDFLAGS="-s -w -X main.versionMajor=${VERSION_MAJOR} -X main.versionMinor=${VERSION_MINOR} -X main.versionPatch=${VERSION_PATCH} -X main.gitCommit=${GIT_COMMIT}"

# Create dist directory
mkdir -p dist

echo "[1/2] Building for Windows (amd64)..."
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="${LDFLAGS}" -o dist/switchai-windows-amd64.exe
if [ $? -ne 0 ]; then
    echo "ERROR: Windows build failed"
    exit 1
fi
echo "Windows build completed: dist/switchai-windows-amd64.exe"

echo ""
echo "[2/3] Building for Linux (amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="${LDFLAGS}" -o dist/switchai-linux-amd64
if [ $? -ne 0 ]; then
    echo "ERROR: Linux amd64 build failed"
    exit 1
fi
echo "Linux build completed: dist/switchai-linux-amd64"

echo ""
echo "[3/3] Building for Linux (aarch64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="${LDFLAGS}" -o dist/switchai-linux-aarch64
if [ $? -ne 0 ]; then
    echo "ERROR: Linux aarch64 build failed"
    exit 1
fi
echo "Linux aarch64 build completed: dist/switchai-linux-aarch64"

echo ""
echo "========================================"
echo "Build completed successfully!"
echo "========================================"
echo ""
echo "Output files:"
echo "  - dist/switchai-windows-amd64.exe"
echo "  - dist/switchai-linux-amd64"
echo "  - dist/switchai-linux-aarch64"
echo ""
echo "Usage:"
echo "  Windows:    switchai-windows-amd64.exe -p 7777"
echo "  Linux amd:  ./switchai-linux-amd64 -p 7777"
echo "  Linux arm:  ./switchai-linux-aarch64 -p 7777"
echo ""
echo "Service management:"
echo "  Install:   switchai-windows-amd64.exe -install"
echo "  Uninstall: switchai-windows-amd64.exe -uninstall"
echo ""

# Make linux binaries executable
chmod +x dist/switchai-linux-amd64 2>/dev/null || true
chmod +x dist/switchai-linux-aarch64 2>/dev/null || true

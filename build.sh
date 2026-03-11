#!/bin/bash
set -e

# Build CLI + UI server
go build -o gitmachine ./cmd/gitmachine

# Build agent (host daemon) for current platform
go build -o gitmachine-agent ./cmd/gitmachine-agent

# Build sandbox daemon (in-container daemon) for current platform
go build -o gitmachine-sandbox-daemon ./cmd/gitmachine-sandbox-daemon

echo "Built: gitmachine, gitmachine-agent, gitmachine-sandbox-daemon"

#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
TMPDIR="${TMPDIR:-$REPO_ROOT/.tmp}"
GOCACHE="${GOCACHE:-$REPO_ROOT/.gocache}"

mkdir -p "$TMPDIR" "$GOCACHE"

which xcaddy >/dev/null 2>&1 || go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

export CGO_ENABLED=1
export TMPDIR
export GOCACHE

xcaddy build \
    --with github.com/mdrv/caddy-duckdb="$REPO_ROOT" \
    --output "$REPO_ROOT/caddy-duckdb"

echo "Built: $REPO_ROOT/caddy-duckdb ($(du -sh "$REPO_ROOT/caddy-duckdb" | cut -f1))"

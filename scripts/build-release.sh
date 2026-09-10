#!/usr/bin/env bash
set -euo pipefail

# One target per runner: avoid competing cross-compilers. Keep release flags
# identical locally and in CI; preserve the existing raw-binary install flow.
: "${GOOS:?GOOS must be set}"
: "${GOARCH:?GOARCH must be set}"
: "${OUTPUT:?OUTPUT must be set}"
VERSION="${VERSION:-dev}"
case "$GOOS/$GOARCH" in
  darwin/amd64|darwin/arm64|linux/amd64|linux/arm64|windows/amd64|windows/arm64) ;;
  *) echo "Unsupported release target: $GOOS/$GOARCH" >&2; exit 1 ;;
esac

export CGO_ENABLED=0
LDFLAGS="-s -w -X github.com/claude-code-launch/ccl/cmd.Version=$VERSION"
if [[ -n "${GOOGLE_OAUTH_CLIENT_SECRET:-}" ]]; then
  LDFLAGS="$LDFLAGS -X github.com/claude-code-launch/ccl/internal/cloudsync.googleOAuthClientSecret=$GOOGLE_OAUTH_CLIENT_SECRET"
fi
mkdir -p "$(dirname "$OUTPUT")"
go build -trimpath -ldflags="$LDFLAGS" -o "$OUTPUT" .

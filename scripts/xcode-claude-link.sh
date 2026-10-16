#!/bin/bash
# Points Xcode's built-in Claude agent at the local claude binary.
#
# Xcode downloads its own agent binary into
#   ~/Library/Developer/Xcode/CodingAssistant/Agents/claude/<version>/claude
# and records a checksum for it. Xcode may re-download the binary when it
# updates the agent, which replaces the link; re-run this script to restore it.
#
# Usage: scripts/xcode-claude-link.sh [path-to-claude]
# Without an argument the claude on PATH is used. The Apple binary is kept
# next to the link as claude.apple. To roll back:
#   mv claude.apple claude   # inside the version directory
#
# Sharing configuration is a separate link (not done here):
#   ln -s ~/.claude ~/Library/Developer/Xcode/CodingAssistant/ClaudeAgentConfig
set -euo pipefail

if [ -z "${HOME:-}" ]; then
    echo "HOME is not set" >&2
    exit 1
fi
agents_root="$HOME/Library/Developer/Xcode/CodingAssistant/Agents/claude"
if [ ! -d "$agents_root" ]; then
    echo "Xcode Claude agent directory not found: $agents_root" >&2
    exit 1
fi

claude_bin="${1:-$(command -v claude || true)}"
if [ -z "$claude_bin" ] || [ ! -x "$claude_bin" ]; then
    echo "claude binary not found; pass its path: $0 /path/to/claude" >&2
    exit 1
fi
# The path is linked as given: /opt/homebrew/bin/claude is itself a symlink
# that keeps working across claude updates, and re-running the script is
# idempotent against exactly that path.

latest=$(
    find "$agents_root" -maxdepth 1 -type d | while IFS= read -r dir; do
        basename "$dir"
    done | grep -E '^[0-9]+([.][0-9]+)*$' |
        awk -F. '{ printf "%06d %06d %06d %06d %s\n", $1, $2, $3, $4, $0 }' |
        sort -r | awk 'NR==1 { print $5 }'
)
if [ -z "$latest" ]; then
    echo "No agent version directory under $agents_root" >&2
    exit 1
fi

dir="$agents_root/$latest"
target="$dir/claude"
if [ -L "$target" ] && [ "$(readlink "$target")" = "$claude_bin" ]; then
    echo "Already linked: $target -> $claude_bin"
    exit 0
fi
if [ -f "$target" ] && [ ! -L "$target" ]; then
    mv -f "$target" "$dir/claude.apple"
fi
ln -sfn "$claude_bin" "$target"
if "$target" --version >/dev/null 2>&1; then
    echo "Linked: $target -> $claude_bin (Apple binary kept as $dir/claude.apple)"
else
    echo "Linked $target -> $claude_bin, but the binary failed to run" >&2
    exit 1
fi

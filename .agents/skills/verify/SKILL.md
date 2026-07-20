---
name: verify
summary: Verify ccl CLI/TUI changes through an isolated HOME and tmux session.
---

1. Build an isolated binary: `go build -o /tmp/ccl-verify .`.
2. Create a temporary HOME with `~/.ccl/config.yaml` containing only fake credentials and mode `0600`.
3. Exercise noninteractive surfaces with `HOME=$tmp /tmp/ccl-verify ls`, `use`, and `preview` (whitelist/redact output).
4. Exercise TUI surfaces in isolated tmux: `tmux -L ccl-verify new-session -d -x 110 -y 35 ...`; drive with `send-keys`, capture with `capture-pane -p`, and strip ANSI for evidence.
5. Never point verification at the real `~/.ccl/config.yaml`; it contains provider credentials.

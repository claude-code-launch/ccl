---
name: verify
summary: Verify ccl CLI/TUI changes through an isolated HOME and tmux session.
---

1. Build an isolated binary: `go build -o /tmp/ccl-verify .` (use `$TMPDIR` when `/tmp` is not writable).
2. Create a temporary HOME with `~/.ccl/config.yaml` containing only fake credentials and mode `0600`. Point the fake provider's `endpoint` at a refused loopback port such as `http://127.0.0.1:9`, so catalog checks fail at once instead of hanging on DNS or a real host.
3. Exercise noninteractive surfaces with `HOME=$tmp /tmp/ccl-verify ls`, `use <name>`, and `doctor` (without `--probe`; whitelist/redact output). Use only subcommands listed by `ccl --help`: any other first argument is forwarded to Claude Code and starts a session.
4. Exercise TUI surfaces in isolated tmux: `tmux -L ccl-verify new-session -d -x 110 -y 35 ...`; drive with `send-keys`, capture with `capture-pane -p`, and strip ANSI for evidence.
5. Never point verification at the real `~/.ccl/config.yaml`; it contains provider credentials.

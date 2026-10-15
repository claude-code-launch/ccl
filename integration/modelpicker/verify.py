"""Check the real Claude /model UI using temporary homes and local fake keys.

No prompt is submitted. The mock endpoint rejects every inference request.
Windows uses ConPTY; pexpect also permits a local Linux/macOS diagnostic run.
"""

import http.server
import json
import os
from pathlib import Path
import queue
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time

import pyte


def main():
    # GitHub's redirected Windows stdout otherwise defaults to cp1252.
    sys.stdout.reconfigure(encoding="utf-8")
    sys.stderr.reconfigure(encoding="utf-8")
    ccl_source = Path(os.environ["CCL_TEST_BINARY"]).resolve()
    claude_source = Path(os.environ["CCL_TEST_CLAUDE"]).resolve()
    evidence = Path(os.environ["CCL_TEST_EVIDENCE"])
    evidence.mkdir(parents=True, exist_ok=True)
    calls = []

    class Upstream(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"data":[],"has_more":false}')

        def do_POST(self):
            calls.append(self.path)
            self.send_response(503)
            self.end_headers()
            self.wfile.write(b"No inference permitted in the model picker check")

        def log_message(self, *_):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    root = Path(tempfile.mkdtemp(prefix="ccl model picker "))
    try:
        binary_dir = root / "Claude install 测试"
        binary_dir.mkdir()
        ccl = binary_dir / ccl_source.name
        claude = binary_dir / ("claude.exe" if os.name == "nt" else "claude")
        shutil.copy2(ccl_source, ccl)
        shutil.copy2(claude_source, claude)
        version = subprocess.check_output([str(claude), "--version"], text=True).strip()
        print(f"Checking {version}", flush=True)
        for mode in ("launch", "proxy"):
            home = root / f"{mode} user 配置"
            project = home / "project"
            config_dir = home / "Claude settings 中文"
            project.mkdir(parents=True)
            config_dir.mkdir()
            (home / ".ccl").mkdir()
            provider = {
                "name": "fixture", "type": "anthropic",
                "endpoint": f"http://127.0.0.1:{server.server_port}",
                "apikey": "fake-local-key",
                "model": "acme/pro,acme/custom,acme/unmapped-模型",
                "opusModel": "acme/pro[1m]", "sonnetModel": "acme/pro",
                "haikuModel": "acme/custom", "fableModel": "acme/pro",
                "customModelId": "acme/custom", "statuslineDisabled": True,
            }
            (home / ".ccl/config.yaml").write_text(json.dumps({
                "lang": "en", "active_provider": "fixture", "providers": {"fixture": provider},
            }), encoding="utf-8")
            (config_dir / ".claude.json").write_text(json.dumps({
                "hasCompletedOnboarding": True, "theme": "dark",
                "lastOnboardingVersion": version.split()[0],
                "customApiKeyResponses": {"approved": ["fake-local-key"]},
                "projects": {key: {"hasTrustDialogAccepted": True,
                                   "hasCompletedProjectOnboarding": True}
                             for path in (project, project.resolve())
                             for key in (str(path), path.as_posix())},
            }), encoding="utf-8")
            env = dict(os.environ, HOME=str(home), USERPROFILE=str(home),
                       CLAUDE_CONFIG_DIR=str(config_dir), CCL_LANG="en", TERM="xterm-256color",
                       PATH=str(binary_dir) + os.pathsep + os.environ["PATH"],
                       CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1", DISABLE_AUTOUPDATER="1")
            args = [str(ccl if mode == "launch" else claude), "--strict-mcp-config",
                    "--mcp-config", '{"mcpServers":{}}']
            child = None
            output = queue.Queue()
            screen = pyte.Screen(160, 50)
            stream = pyte.Stream(screen)
            transcript = []
            trust_answered = False

            def read_terminal(process):
                try:
                    while True:
                        chunk = process.read() if os.name == "nt" else process.read_nonblocking(65536, 30)
                        if not chunk:
                            break
                        output.put(chunk)
                except (EOFError, OSError):
                    pass
                except Exception as error:
                    output.put(str(error))

            def wait_for(predicate):
                nonlocal trust_answered
                deadline = time.monotonic() + 45
                while time.monotonic() < deadline:
                    try:
                        chunk = output.get(timeout=0.1)
                    except queue.Empty:
                        continue
                    transcript.append(chunk)
                    stream.feed(chunk)
                    text = "\n".join(screen.display)
                    if not trust_answered and "Yes, I trust this folder" in text:
                        # Claude normalizes Windows project paths differently
                        # across versions. Trust only our newly created fixture.
                        trust_answered = True
                        child.write("\x1b[B")
                        time.sleep(0.2)
                        child.write("\r")
                    if predicate(text):
                        return text
                print("\n".join(screen.display).strip(), flush=True)
                raise AssertionError("Timed out waiting for Claude terminal; see evidence")

            try:
                if mode == "proxy":
                    subprocess.run([str(ccl), "proxy", "-p", "fixture"], env=env, cwd=project,
                                   check=True, timeout=30, capture_output=True)
                if os.name == "nt":
                    from winpty import PtyProcess
                    child = PtyProcess.spawn(args, cwd=str(project), env=env, dimensions=(50, 160))
                else:
                    import pexpect
                    child = pexpect.spawn(args[0], args[1:], cwd=str(project), env=env,
                                          dimensions=(50, 160), encoding="utf-8")
                threading.Thread(target=read_terminal, args=(child,), daemon=True).start()
                wait_for(lambda text: "acme/custom" in text and "❯" in text)
                child.write("/model")
                time.sleep(0.5)
                child.write("\r")
                expected = ["Select model", "Opus · acme/pro (1M)", "Sonnet · acme/pro",
                            "Custom · acme/custom", "acme/unmapped-模型"]
                text = wait_for(lambda text: all(label in text for label in expected))
                # Haiku shares the Custom model and Fable shares Sonnet's, so
                # each model is listed once.
                for label in ("Haiku · acme/custom", "Fable · acme/pro"):
                    assert label not in text, f"Duplicate model picker row: {label}"
                assert "�" not in text, "Invalid Unicode in model picker"
                assert not calls, f"Unexpected inference calls: {calls}"
                print(f"PASS {mode}: real /model tier, Custom, unmapped Unicode and 1M labels", flush=True)
            finally:
                (evidence / f"{mode}-screen.txt").write_text("\n".join(screen.display), encoding="utf-8")
                (evidence / f"{mode}-terminal.txt").write_text("".join(transcript), encoding="utf-8")
                if child is not None:
                    if os.name == "nt":
                        subprocess.run(["taskkill", "/pid", str(child.pid), "/t", "/f"], capture_output=True)
                    else:
                        try:
                            os.killpg(os.getpgid(child.pid), signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                if mode == "proxy":
                    subprocess.run([str(ccl), "proxy", "stop"], env=env, cwd=project,
                                   check=True, timeout=30, capture_output=True)
    finally:
        server.shutdown()
        shutil.rmtree(root, ignore_errors=True)


if __name__ == "__main__":
    main()

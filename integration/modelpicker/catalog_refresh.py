#!/usr/bin/env python3
"""Qualify Codex catalog refresh with a temporary profile and metadata-only server.

CCL_TEST_CODEX=/path/to/codex python3 integration/modelpicker/catalog_refresh.py
The default idle period covers the observed 270-second worker and 300-second TTL.
Use --idle-seconds 0 for the immediate cache/configuration cases only.
"""

import argparse
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def notification_methods(value):
    methods = set()
    if isinstance(value, dict):
        method = value.get("properties", {}).get("method", {})
        methods.update(method.get("enum", []))
        if "const" in method:
            methods.add(method["const"])
        for nested in value.values():
            methods.update(notification_methods(nested))
    elif isinstance(value, list):
        for nested in value:
            methods.update(notification_methods(nested))
    return methods


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", default=os.environ.get("CCL_TEST_CODEX"))
    parser.add_argument("--idle-seconds", type=float, default=306)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if not args.codex or args.idle_seconds < 0:
        parser.error("provide --codex or CCL_TEST_CODEX and a nonnegative idle period")
    executable = str(Path(args.codex).resolve())
    started = time.monotonic()
    lock = threading.Lock()
    state = {"a": "model-A", "b": "model-B", "requests": []}
    report = {"steps": [], "notifications": [], "inference_requests": 0}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            with lock:
                family = "b" if self.path.startswith("/models-b") else "a"
                slug = state[family]
                state["requests"].append({
                    "path": self.path, "seconds": round(time.monotonic() - started, 2)
                })
            model = {
                "slug": slug, "display_name": slug, "description": "Local catalog fixture",
                "context_window": 200000, "max_context_window": 200000,
                "shell_type": "default", "visibility": "list", "supported_in_api": True,
                "priority": 0, "truncation_policy": {"mode": "tokens", "limit": 10000},
                "input_modalities": ["text"], "base_instructions": "Local fixture",
                "support_verbosity": False, "default_verbosity": None,
                "supports_parallel_tool_calls": False, "supports_reasoning_summaries": False,
                "supported_reasoning_levels": [], "default_reasoning_level": None,
                "apply_patch_tool_type": "freeform", "experimental_supported_tools": [],
            }
            raw = json.dumps({"models": [model]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("ETag", json.dumps(slug))
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_POST(self):
            with lock:
                report["inference_requests"] += 1
            self.send_error(500, "Inference is disabled in this fixture")

        def log_message(self, *_):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="ccl-catalog-refresh-") as home:
            base = f"http://127.0.0.1:{server.server_port}"
            Path(home, "config.toml").write_text(
                'model_provider="fixture"\nmodel="model-A"\n'
                '[features]\napi_key_model_discovery=true\n'
                '[model_providers.fixture]\nname="Local fixture"\n'
                f'base_url="{base}/v1"\nmodel_catalog_url="{base}/models-a"\n'
                'wire_api="responses"\nrequires_openai_auth=false\n'
                'supports_websockets=false\nexperimental_bearer_token="fake-local-key"\n'
            )
            env = {key: value for key, value in os.environ.items() if key not in {
                "HOME", "USERPROFILE", "CODEX_HOME", "OPENAI_API_KEY", "CODEX_API_KEY"
            }}
            env.update(
                HOME=home, USERPROFILE=home, CODEX_HOME=home,
                HTTP_PROXY="http://127.0.0.1:9", HTTPS_PROXY="http://127.0.0.1:9",
                ALL_PROXY="http://127.0.0.1:9", NO_PROXY="127.0.0.1,localhost",
            )
            report["client"] = subprocess.check_output(
                [executable, "--version"], cwd=home, env=env, text=True, timeout=15
            ).strip()
            schema = Path(home, "schema")
            subprocess.run(
                [executable, "app-server", "generate-json-schema", "--out", str(schema)],
                cwd=home, env=env, check=True, capture_output=True, timeout=20,
            )
            params = json.loads((schema / "v2/ModelListParams.json").read_text())
            report["model_list_parameters"] = sorted(params.get("properties", {}))
            notifications = json.loads((schema / "ServerNotification.json").read_text())
            report["model_notifications_in_schema"] = sorted(
                method for method in notification_methods(notifications)
                if "model" in method.lower() or "catalog" in method.lower()
            )
            proc = subprocess.Popen(
                [executable, "app-server"], cwd=home, env=env, text=True,
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                start_new_session=os.name != "nt",
            )
            messages = queue.Queue()

            def read():
                for line in proc.stdout:
                    try:
                        message = json.loads(line)
                    except ValueError:
                        continue
                    if "id" not in message and message.get("method"):
                        with lock:
                            report["notifications"].append(message["method"])
                    messages.put(message)

            threading.Thread(target=read, daemon=True).start()

            def rpc(ident, method, params):
                proc.stdin.write(json.dumps({"id": ident, "method": method, "params": params}) + "\n")
                proc.stdin.flush()
                while True:
                    response = messages.get(timeout=15)
                    if response.get("id") == ident:
                        if "error" in response:
                            raise RuntimeError(f"{method}: {response['error']}")
                        return response.get("result", {})

            def models(ident, label):
                result = rpc(ident, "model/list", {"includeHidden": True, "limit": 100})
                with lock:
                    row = {"step": label, "models": [item["model"] for item in result["data"]],
                           "fetches": len(state["requests"])}
                    report["steps"].append(row)
                print(json.dumps(row), flush=True)

            def config_url(ident, path):
                rpc(ident, "config/batchWrite", {
                    "edits": [{"keyPath": "model_providers.fixture.model_catalog_url",
                               "value": base + path, "mergeStrategy": "replace"}],
                    "reloadUserConfig": True,
                })

            try:
                rpc(0, "initialize", {"clientInfo": {"name": "ccl-fixture", "version": "1"},
                                      "capabilities": {"experimentalApi": True}})
                proc.stdin.write(json.dumps({"method": "initialized"}) + "\n")
                proc.stdin.flush()
                models(1, "initial")
                with lock:
                    state["a"] = "model-A-changed"
                models(2, "same process after catalog edit")
                config_url(3, "/models-b")
                models(4, "after catalog URL edit with reloadUserConfig")
                config_url(5, "/models-a")
                models(6, "after restoring catalog URL")
                with lock:
                    state["a"], state["b"] = "model-after-idle", "model-after-idle-B"
                    before = len(state["requests"])
                idle_started = time.monotonic()
                print(f"Observing {args.idle_seconds:g} seconds without RPC calls", flush=True)
                while time.monotonic() - idle_started < args.idle_seconds:
                    time.sleep(max(0, min(1, args.idle_seconds - (time.monotonic() - idle_started))))
                with lock:
                    report["idle_seconds"] = round(time.monotonic() - idle_started, 2)
                    report["idle_fetches"] = len(state["requests"]) - before
                models(7, "same process after idle")
            finally:
                if os.name != "nt":
                    try:
                        os.killpg(proc.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
                else:
                    proc.terminate()
                try:
                    proc.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
    finally:
        server.shutdown()
        server.server_close()
        with lock:
            report["requests"] = list(state["requests"])
        raw = json.dumps(report, indent=2)
        if args.output:
            args.output.write_text(raw + "\n")
        print(raw)
    if report["inference_requests"]:
        raise RuntimeError("metadata-only probe attempted inference")


if __name__ == "__main__":
    main()

import json
import os
import subprocess
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOST = os.environ.get("BASH_TOOL_HOST", "0.0.0.0")
PORT = int(os.environ.get("BASH_TOOL_PORT", "8080"))
DEFAULT_TIMEOUT_MS = int(os.environ.get("BASH_TOOL_DEFAULT_TIMEOUT_MS", "120000"))
MAX_TIMEOUT_MS = int(os.environ.get("BASH_TOOL_MAX_TIMEOUT_MS", "600000"))
DEFAULT_WORKDIR = os.environ.get("BASH_TOOL_DEFAULT_WORKDIR", "/workspace")

os.makedirs(DEFAULT_WORKDIR, exist_ok=True)


def respond(handler, code, payload):
    body = json.dumps(payload).encode("utf-8")
    handler.send_response(code)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        return

    def do_GET(self):
        if self.path == "/health":
            return respond(self, 200, {"ok": True})
        return respond(self, 404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/exec":
            return respond(self, 404, {"error": "not found"})

        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            return respond(self, 400, {"error": "invalid content length"})

        try:
            raw = self.rfile.read(length)
            payload = json.loads(raw or b"{}")
        except Exception as exc:
            return respond(self, 400, {"error": f"invalid json: {exc}"})

        command = str(payload.get("command", "")).strip()
        if not command:
            return respond(self, 400, {"error": "command is required"})

        working_dir = str(payload.get("working_dir") or DEFAULT_WORKDIR).strip() or DEFAULT_WORKDIR
        timeout_ms = payload.get("timeout_ms", DEFAULT_TIMEOUT_MS)
        try:
            timeout_ms = int(timeout_ms)
        except Exception:
            return respond(self, 400, {"error": "timeout_ms must be an integer"})

        if timeout_ms <= 0:
            timeout_ms = DEFAULT_TIMEOUT_MS
        if timeout_ms > MAX_TIMEOUT_MS:
            timeout_ms = MAX_TIMEOUT_MS

        os.makedirs(working_dir, exist_ok=True)

        started = time.time()
        try:
            proc = subprocess.run(
                ["/bin/bash", "-lc", command],
                cwd=working_dir,
                capture_output=True,
                text=True,
                timeout=timeout_ms / 1000.0,
                env=os.environ.copy(),
            )
            return respond(self, 200, {
                "command": command,
                "working_dir": working_dir,
                "exit_code": proc.returncode,
                "stdout": proc.stdout,
                "stderr": proc.stderr,
                "duration_ms": int((time.time() - started) * 1000),
                "timed_out": False,
            })
        except subprocess.TimeoutExpired as exc:
            return respond(self, 200, {
                "command": command,
                "working_dir": working_dir,
                "exit_code": None,
                "stdout": exc.stdout or "",
                "stderr": exc.stderr or "",
                "duration_ms": int((time.time() - started) * 1000),
                "timed_out": True,
            })
        except Exception as exc:
            return respond(self, 500, {"error": str(exc)})


if __name__ == "__main__":
    server = ThreadingHTTPServer((HOST, PORT), Handler)
    server.serve_forever()

from __future__ import annotations

import json
import os
import subprocess
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import cast

CONTEXT = os.environ["SANDBOX_K8S_CONTEXT"]
NAMESPACE = os.environ["SANDBOX_K8S_NAMESPACE"]
POOLS_VALUE: object = json.loads(os.environ["SANDBOX_K8S_POOLS"])
if not isinstance(POOLS_VALUE, dict):
    raise ValueError("SANDBOX_K8S_POOLS must be an object")
POOLS = cast(dict[object, object], POOLS_VALUE)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format: str, *args: object) -> None:
        del format, args

    def do_GET(self) -> None:
        prefix = "/ready/"
        if not self.path.startswith(prefix):
            self._send(404, {"error": "not found"})
            return
        pool = self.path[len(prefix) :]
        config = POOLS.get(pool)
        if not isinstance(config, dict):
            self._send(404, {"error": "unknown pool"})
            return
        typed_config = cast(dict[object, object], config)
        name = typed_config.get("warmPoolName")
        if not isinstance(name, str):
            self._send(404, {"error": "unknown pool"})
            return
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            value = subprocess.run(
                [
                    "kubectl",
                    "--context",
                    CONTEXT,
                    "-n",
                    NAMESPACE,
                    "get",
                    "sandboxwarmpool",
                    name,
                    "-o",
                    "json",
                ],
                check=True,
                capture_output=True,
                text=True,
            )
            resource = json.loads(value.stdout)
            desired = resource.get("spec", {}).get("replicas", 0)
            ready = resource.get("status", {}).get("readyReplicas", 0)
            if desired > 0 and ready >= desired:
                self._send(200, {"pool": pool, "desired": desired, "ready": ready})
                return
            time.sleep(0.2)
        self._send(503, {"error": "WarmPool did not become ready", "pool": pool})

    def _send(self, status: int, payload: object) -> None:
        encoded = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)


ThreadingHTTPServer(("127.0.0.1", int(os.environ.get("SANDBOX_BENCHMARK_OBSERVER_PORT", "18792"))), Handler).serve_forever()

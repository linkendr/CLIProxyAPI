"""Fake upstream for the reserve-gate smoke test. Loopback only, no credentials.

POST /v1/messages        Anthropic Messages reply with unified rate-limit headers
                         (5h utilization from argv[2]).
POST /codex/responses    Codex-style Responses SSE stream ending in response.completed.
Every hit is appended to argv[3] as "<path> <x-api-key or bearer suffix>".
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
UTIL = sys.argv[2]
HITS = sys.argv[3]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _record(self):
        with open(HITS, "a", encoding="utf-8") as f:
            f.write(self.path + "\n")

    def do_GET(self):
        body = b"ok"
        self.send_response(200)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("content-length") or 0)
        self.rfile.read(n)
        self._record()
        if self.path.startswith("/v1/messages"):
            self._anthropic()
        elif self.path.startswith("/codex/responses"):
            self._codex()
        else:
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()

    def _anthropic(self):
        body = json.dumps({
            "id": "msg_smoke", "type": "message", "role": "assistant", "model": "claude-smoke",
            "content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
            "usage": {"input_tokens": 1, "output_tokens": 1},
        }).encode()
        now = int(time.time())
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("anthropic-ratelimit-unified-status", "allowed")
        self.send_header("anthropic-ratelimit-unified-5h-utilization", UTIL)
        self.send_header("anthropic-ratelimit-unified-5h-reset", str(now + 3600))
        self.send_header("anthropic-ratelimit-unified-5h-status", "allowed")
        self.send_header("anthropic-ratelimit-unified-7d-utilization", "0.10")
        self.send_header("anthropic-ratelimit-unified-7d-reset", str(now + 5 * 86400))
        self.send_header("anthropic-ratelimit-unified-7d-status", "allowed")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _codex(self):
        item = {"id": "msg_codex_smoke", "type": "message", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": "ok", "annotations": []}]}
        response = {"id": "resp_codex_smoke", "object": "response", "created_at": int(time.time()),
                    "status": "completed", "model": "gpt-smoke", "output": [item],
                    "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
        events = [
            {"type": "response.created", "response": dict(response, status="in_progress", output=[])},
            {"type": "response.output_item.done", "output_index": 0, "item": item},
            {"type": "response.completed", "response": response},
        ]
        body = "".join("event: %s\ndata: %s\n\n" % (e["type"], json.dumps(e)) for e in events).encode()
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()

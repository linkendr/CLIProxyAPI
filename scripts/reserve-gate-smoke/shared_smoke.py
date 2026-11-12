"""Exercise the shared reserve and the credit-key fallback of a built proxy over loopback.

Fake credentials only: priority-0 keys stand in for Max logins and a priority -10
fallback-when key stands in for a Console credit key. The fake upstream reports only the
7d window, as Anthropic does now. Each scenario runs a fresh proxy.
"""

import argparse
import http.server
import json
import pathlib
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request

LIMIT_RETRY_SECONDS = 11  # just above the proxy's 10 s quota cooldown floor
# The Claude executor adds a random 1-30 s grace to every rate-limit cooldown
# (defaultClaudeRateLimitFuzzMaxSeconds in internal/runtime/executor/helps/claude_ratelimit.go).
COOLDOWN_GRACE_MAX_SECONDS = 30
HOUR = 3600
DAY = 24 * HOUR


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Fixture:
    """One fake upstream and one proxy with the given Max stand-ins and a credit key."""

    def __init__(self, binary, run, maxes):
        self.run = run
        run.mkdir(parents=True, exist_ok=False)
        (run / "auth").mkdir()
        (run / "home").mkdir()
        # Per Max stand-in: 7d reading and whether it answers with a real limit.
        self.max = {name: {"7d": (0.10, int(time.time()) + 2 * DAY), "limited": False} for name in maxes}
        self.credit_empty = False
        self.hits = []
        fixture = self

        class Upstream(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("content-length", 0)))
                route = self.path.split("/")[1]
                fixture.hits.append(route)
                if route == "credit":
                    if fixture.credit_empty:
                        self.reply(400, {"type": "error", "error": {"type": "invalid_request_error",
                                                                     "message": "Your credit balance is too low to access the Anthropic API."}}, {})
                    else:
                        self.reply(200, fixture.message(route), {})
                    return
                state = fixture.max[route]
                if state.get("steer"):
                    # A short plain rate limit with no weekly headers: the proxy cools the
                    # login for retry-after only, not until its weekly reset.
                    self.reply(429, {"type": "error", "error": {"type": "rate_limit_error", "message": "busy"}},
                               {"retry-after": str(LIMIT_RETRY_SECONDS)})
                    return
                usage, reset = state["7d"]
                status = "rejected" if state["limited"] else "allowed"
                headers = {"anthropic-ratelimit-unified-status": status,
                           "anthropic-ratelimit-unified-7d-utilization": str(usage),
                           "anthropic-ratelimit-unified-7d-reset": str(reset),
                           "anthropic-ratelimit-unified-7d-status": status}
                if state["limited"]:
                    headers["retry-after"] = str(LIMIT_RETRY_SECONDS)
                    self.reply(429, {"type": "error", "error": {"type": "rate_limit_error", "message": "limit"}}, headers)
                    return
                self.reply(200, fixture.message(route), headers)

            def reply(self, code, payload, headers):
                body = json.dumps(payload).encode()
                self.send_response(code)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(body)))
                for name, value in headers.items():
                    self.send_header(name, value)
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *unused):
                pass

        self.upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        threading.Thread(target=self.upstream.serve_forever, daemon=True).start()
        self.port = free_port()
        base = f"http://127.0.0.1:{self.upstream.server_port}"
        entries = "".join(f"""    - name: {name}-fake
      base-url: "{base}/{name}"
      keys:
        - api-key: "{name}-upstream-fake"
      models:
        - name: "claude-smoke"
""" for name in maxes)
        config = run / "config.yaml"
        config.write_text(f"""config-version: 8
server:
  host: "127.0.0.1"
  port: {self.port}
access:
  api-keys: [shared-user-fake, shared-automation-fake]
oauth:
  auth-dir: "{run / 'auth'}"
remote-management:
  disable-control-panel: true
  disable-auto-update-panel: true
routing:
  strategy: pace
  reserve-gate:
    enabled: true
    threshold: 0.95
    exempt-api-keys: [shared-automation-fake]
api-keys:
  claude:
{entries}    - name: credit-fake
      base-url: "{base}/credit"
      priority: -10
      fallback-when: limit
      keys:
        - api-key: "credit-upstream-fake"
      models:
        - name: "claude-smoke"
""")
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.log_path = run / "proxy.log"
        self.log = self.log_path.open("wb")
        self.process = subprocess.Popen([str(binary), "--config", str(config)], cwd=run,
                                        env={"HOME": str(run / "home"), "PATH": "/usr/bin:/bin"},
                                        stdout=self.log, stderr=subprocess.STDOUT)
        for _ in range(200):
            if self.process.poll() is not None:
                raise RuntimeError(f"proxy exited {self.process.returncode}: see {self.log_path}")
            try:
                with self.opener.open(f"http://127.0.0.1:{self.port}/", timeout=1):
                    return
            except (OSError, urllib.error.URLError):
                time.sleep(0.1)
        raise RuntimeError("fixture proxy did not start")

    @staticmethod
    def message(route):
        return {"id": "msg_shared", "type": "message", "role": "assistant", "model": "claude-smoke",
                "content": [{"type": "text", "text": route}], "stop_reason": "end_turn",
                "usage": {"input_tokens": 1, "output_tokens": 1}}

    def request(self, automation=False):
        key = "shared-automation-fake" if automation else "shared-user-fake"
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}/v1/messages", method="POST",
                                     headers={"Authorization": f"Bearer {key}", "content-type": "application/json",
                                              "anthropic-version": "2023-06-01"},
                                     data=json.dumps({"model": "claude-smoke", "max_tokens": 16,
                                                      "messages": [{"role": "user", "content": "hi"}]}).encode())
        try:
            response = self.opener.open(req, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.read().decode(), response.headers

    def check(self, name, expected_status, expected_routes, automation=False, reserve=False):
        """expected_routes: exact list, or a set of routes that must all be absent."""
        before = len(self.hits)
        status, body, headers = self.request(automation)
        routes = self.hits[before:]
        ok = (status == expected_status if expected_status else status != 200)
        if isinstance(expected_routes, set):
            ok = ok and not expected_routes.intersection(routes)
        else:
            ok = ok and routes == expected_routes
        if reserve:
            ok = ok and "quota reserve hold" in body
        if not ok:
            raise AssertionError(f"{name}: HTTP {status} routes {routes}, expected {expected_status} {expected_routes}: {body}")
        retry = headers.get("Retry-After")
        print(f"PASS {name}: HTTP {status} via {routes}" + (f" Retry-After={retry}" if retry else ""), flush=True)
        return status, body, headers

    def record(self, names, attempts=6):
        """Automation requests until every named stand-in was reached at least once. Pace
        keeps sending automation to one login, so a stand-in already reached answers with
        a short plain 429 (its reading stays known) until the request moves on; afterwards
        the 429s stop and the proxy's short cooldown is waited out."""
        steered = []
        for _ in range(attempts):
            missing = [name for name in names if name not in self.hits]
            if not missing:
                break
            for name in names:
                if name in self.hits and not self.max[name]["limited"] and not self.max[name].get("steer"):
                    self.max[name]["steer"] = True
                    steered.append(name)
            self.request(True)
        if not set(names) <= set(self.hits):
            raise AssertionError(f"automation did not reach {names}: {self.hits}")
        print(f"PASS automation recorded readings of {sorted(names)} via {self.hits}", flush=True)
        if steered:
            for name in steered:
                self.max[name]["steer"] = False
            time.sleep(LIMIT_RETRY_SECONDS + COOLDOWN_GRACE_MAX_SECONDS + 2)

    def reserve_log(self):
        self.log.flush()
        return [line for line in self.log_path.read_text(errors="replace").splitlines() if "reserve gate:" in line]

    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            self.process.wait(timeout=15)
        self.upstream.shutdown()
        self.upstream.server_close()
        self.log.close()


def one_held_one_room(binary, run):
    """(a) maxa held at 97% two hours before its reset, maxb at 30% with six days left:
    ordinary callers are served, by maxa, which pace spends first."""
    f = Fixture(binary, run, ["maxa", "maxb"])
    try:
        now = int(time.time())
        f.max["maxa"]["7d"] = (0.97, now + 2 * HOUR)
        f.max["maxb"]["7d"] = (0.30, now + 6 * DAY)
        f.record(["maxa", "maxb"])
        held = [line for line in f.reserve_log() if "7d window held" in line]
        if not held:
            raise AssertionError(f"no hold logged: {f.reserve_log()}")
        print(f"PASS hold logged: {held[-1].split('reserve gate:')[1].strip()}", flush=True)
        f.check("one held, one with room: ordinary served by held maxa", 200, ["maxa"])
        f.check("one held, one with room: ordinary served again", 200, ["maxa"])
        f.check("automation served", 200, {"credit"}, True)
    finally:
        f.close()


def both_held(binary, run):
    """(b) both at 96%: ordinary 429 reserve hold with Retry-After at the earliest reset;
    automation 200."""
    f = Fixture(binary, run, ["maxa", "maxb"])
    try:
        now = int(time.time())
        f.max["maxa"]["7d"] = (0.96, now + 3 * DAY)
        f.max["maxb"]["7d"] = (0.96, now + 4 * DAY)
        f.record(["maxa", "maxb"])
        _, _, headers = f.check("both held: ordinary 429 quota reserve hold, no upstream call", 429, [], reserve=True)
        retry = int(headers.get("Retry-After", "0"))
        if abs(retry - 3 * DAY) > 120:
            raise AssertionError(f"Retry-After {retry}, want about {3 * DAY} (maxa's reset)")
        print(f"PASS Retry-After {retry}s is maxa's reset (earliest)", flush=True)
        f.check("both held: automation 200", 200, {"credit"}, True)
    finally:
        f.close()


def limited_and_held(binary, run):
    """(c) maxa at a real limit, maxb held at 96%: ordinary 429 reserve hold, no credit."""
    f = Fixture(binary, run, ["maxa", "maxb"])
    try:
        now = int(time.time())
        f.max["maxa"].update({"7d": (1.0, now + 3 * DAY), "limited": True})
        f.max["maxb"]["7d"] = (0.96, now + 4 * DAY)
        f.record(["maxa", "maxb"])
        f.check("maxa limited, maxb held: ordinary 429 reserve hold, no credit route", 429, [], reserve=True)
        f.check("automation served by held maxb", 200, ["maxb"], True)
    finally:
        f.close()


def both_limited(binary, run):
    """(d) both at a real limit: ordinary served by the credit key. Then the credit key's
    empty balance is refused and the key cools down."""
    f = Fixture(binary, run, ["maxa", "maxb"])
    try:
        now = int(time.time())
        f.max["maxa"].update({"7d": (1.0, now + 3 * DAY), "limited": True})
        f.max["maxb"].update({"7d": (1.0, now + 4 * DAY), "limited": True})
        f.record(["maxa", "maxb"])
        f.check("both limited: ordinary served by credit key", 200, ["credit"])
        f.check("both limited: automation served by credit key", 200, ["credit"], True)
        f.credit_empty = True
        f.check("empty credit balance refused", None, ["credit"])
        f.check("empty credit key cools down", None, {"credit"})
    finally:
        f.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=pathlib.Path, required=True)
    parser.add_argument("--run-dir", type=pathlib.Path, required=True)
    args = parser.parse_args()
    run = args.run_dir.resolve()
    run.mkdir(parents=True, exist_ok=False)
    binary = args.binary.resolve()
    one_held_one_room(binary, run / "a-one-held")
    both_held(binary, run / "b-both-held")
    limited_and_held(binary, run / "c-limited-held")
    both_limited(binary, run / "d-both-limited")
    print("ALL PASS: shared reserve and credit fallback through real HTTP", flush=True)


if __name__ == "__main__":
    main()

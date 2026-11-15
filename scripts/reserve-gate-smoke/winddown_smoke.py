"""Exercise the reserve wind-down of a built proxy through real HTTP, with fake OAuth Max logins.

Runs inside a disposable Linux container with no network (see winddown_smoke.sh). Max logins
are subscription (OAuth) logins, and an OAuth login always talks to https://api.anthropic.com,
so api.anthropic.com must resolve to 127.0.0.1 (the wrapper adds it to /etc/hosts). This script
makes a throwaway CA, serves a fake HTTPS Anthropic upstream on 127.0.0.1:443 with a
certificate from it, and starts the proxy with SSL_CERT_FILE pointing at the CA. Fake
credentials only: two OAuth login files (maxa, maxb) and Console-style API keys on a plain
HTTP fake (credit: fallback-when limit; drained: priority 10 without fallback-when, as the
credit expiry drain leaves a key). The fake upstream reports only the 7d window, as Anthropic
does now. Held readings are seeded through the gate's state file. Each scenario runs a
fresh proxy in a fresh folder.

The fake upstream also serves the usage report of a login (GET /api/oauth/usage), which the
proxy reads when no Max login can serve a caller. It reports what the fake holds for that
login unless a scenario makes it fail, and it counts the reads per login.
"""

import argparse
import http.server
import json
import os
import pathlib
import re
import socket
import ssl
import subprocess
import threading
import time
import urllib.error
import urllib.request

MODEL = "claude-sonnet-4-6"
NOTE = ("Quota reserve reached. Finish the current step, write a short status of what is done "
        "and what is left, and end your turn. Do not start new tool calls or subagents.")
LIMIT_RETRY_SECONDS = 11
USAGE_PATH = "/api/oauth/usage?cedar_ember=1&skip_spend=1"
USAGE_BETA = "oauth-2025-04-20"
USAGE_READ_SECONDS = 60  # the proxy reads one login's usage at most once in this time
# The countdown at the end of a refusal message.
RESET_AFTER = re.compile(r"\(reset after (?:(\d+)h ?)?(?:(\d+)m ?)?(?:(\d+)s)?\)")
HOUR = 3600
DAY = 24 * HOUR
TOOLS = [{"name": "Bash", "description": "Run a command", "input_schema": {"type": "object", "properties": {}}}]
NEW_PROMPT = {"model": MODEL, "max_tokens": 16, "tools": TOOLS,
              "messages": [{"role": "user", "content": "start a task"}]}
RUNNING_TASK = {"model": MODEL, "max_tokens": 16, "tools": TOOLS, "messages": [
    {"role": "user", "content": "start a task"},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_smoke1", "name": "Bash", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_smoke1", "content": "ok"}]}]}
OPENAI_RUNNING_TASK = {"model": MODEL, "max_tokens": 16, "messages": [
    {"role": "user", "content": "start a task"},
    {"role": "assistant", "content": None, "tool_calls": [
        {"id": "call_smoke1", "type": "function", "function": {"name": "Bash", "arguments": "{}"}}]},
    {"role": "tool", "tool_call_id": "call_smoke1", "content": "ok"}],
    "tools": [{"type": "function", "function": {"name": "Bash", "parameters": {"type": "object", "properties": {}}}}]}


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def make_ca(folder):
    """A throwaway CA and an api.anthropic.com certificate signed by it."""
    folder.mkdir()
    run = lambda *args: subprocess.run(args, cwd=folder, check=True, capture_output=True)
    run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-subj", "/CN=winddown smoke CA",
        "-keyout", "ca.key", "-out", "ca.pem")
    run("openssl", "req", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=api.anthropic.com",
        "-keyout", "server.key", "-out", "server.csr")
    (folder / "ext.cnf").write_text("subjectAltName=DNS:api.anthropic.com\nextendedKeyUsage=serverAuth\n")
    run("openssl", "x509", "-req", "-in", "server.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial",
        "-days", "2", "-extfile", "ext.cnf", "-out", "server.pem")
    return folder / "ca.pem", folder / "server.pem", folder / "server.key"


class Upstream:
    """Fake Anthropic: OAuth logins over TLS on 127.0.0.1:443, API keys over plain HTTP."""

    def __init__(self, certs):
        self.max = {}
        self.credit_empty = False
        self.hits = []  # (route, request body)
        self.usage_fail = set()  # logins whose usage read answers HTTP 500
        self.usage_hits = []  # (login, path, anthropic-beta header)
        self.lock = threading.Lock()
        upstream = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path.startswith("/api/oauth/usage"):
                    name = self.headers.get("authorization", "").rsplit("-", 1)[-1]
                    with upstream.lock:
                        upstream.usage_hits.append((name, self.path, self.headers.get("anthropic-beta", "")))
                        state, failing = upstream.max.get(name), name in upstream.usage_fail
                    if state is None or failing:
                        self.reply(500, {"type": "error", "error": {"type": "api_error", "message": "usage unavailable"}}, {})
                        return
                    usage, reset = state["7d"]
                    percent = 100.0 if state["limited"] else round(usage * 100, 6)
                    stamp = time.strftime("%Y-%m-%dT%H:%M:%S+00:00", time.gmtime(reset))
                    self.reply(200, {"five_hour": None, "seven_day": {"utilization": percent, "resets_at": stamp},
                                     "limits": [{"kind": "weekly_all", "percent": percent, "resets_at": stamp}]}, {})
                    return
                # OAuth profile lookups, should the proxy make one.
                self.reply(200, {"account": {"uuid": "00000000-0000-4000-8000-000000000000", "email": "x@example.invalid"},
                                 "organization": {"uuid": "00000000-0000-4000-8000-000000000001", "name": "smoke"}}, {})

            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("content-length", 0)))
                route = self.path.split("/")[1] if not self.path.startswith("/v1/") else None
                if route is None:  # OAuth login: tell them apart by their fake access token
                    token = self.headers.get("authorization", "").rsplit("-", 1)[-1]
                    route = token
                try:
                    payload = json.loads(body)
                except ValueError:
                    payload = {}
                with upstream.lock:
                    upstream.hits.append((route, payload))
                if route in ("credit", "drained"):
                    if route == "credit" and upstream.credit_empty:
                        self.reply(400, {"type": "error", "error": {"type": "invalid_request_error",
                                         "message": "Your credit balance is too low to access the Anthropic API."}}, {})
                        return
                    self.answer(payload, route, {})
                    return
                state = upstream.max.get(route)
                if state is None:
                    self.reply(401, {"type": "error", "error": {"type": "authentication_error", "message": "unknown"}}, {})
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
                self.answer(payload, route, headers)

            def answer(self, payload, route, headers):
                if payload.get("stream"):
                    events = [
                        ("message_start", {"type": "message_start", "message": {
                            "id": "msg_smoke", "type": "message", "role": "assistant", "model": MODEL, "content": [],
                            "stop_reason": None, "usage": {"input_tokens": 1, "output_tokens": 0}}}),
                        ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
                        ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": route}}),
                        ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                        ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 1}}),
                        ("message_stop", {"type": "message_stop"})]
                    data = "".join(f"event: {name}\ndata: {json.dumps(event)}\n\n" for name, event in events).encode()
                    self.send_response(200)
                    self.send_header("content-type", "text/event-stream")
                    self.send_header("content-length", str(len(data)))
                    for name, value in headers.items():
                        self.send_header(name, value)
                    self.end_headers()
                    self.wfile.write(data)
                    return
                self.reply(200, {"id": "msg_smoke", "type": "message", "role": "assistant", "model": MODEL,
                                 "content": [{"type": "text", "text": route}], "stop_reason": "end_turn",
                                 "usage": {"input_tokens": 1, "output_tokens": 1}}, headers)

            def reply(self, code, payload, headers):
                data = json.dumps(payload).encode()
                self.send_response(code)
                self.send_header("content-type", "application/json")
                self.send_header("content-length", str(len(data)))
                for name, value in headers.items():
                    self.send_header(name, value)
                self.end_headers()
                self.wfile.write(data)

            def log_message(self, *unused):
                pass

        ca, cert, key = certs
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cert, key)
        context.set_alpn_protocols(["http/1.1"])
        self.tls = http.server.ThreadingHTTPServer(("127.0.0.1", 443), Handler)
        self.tls.socket = context.wrap_socket(self.tls.socket, server_side=True)
        self.plain = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        for server in (self.tls, self.plain):
            threading.Thread(target=server.serve_forever, daemon=True).start()
        self.base = f"http://127.0.0.1:{self.plain.server_port}"

    def reset(self, maxes):
        with self.lock:
            self.max = {name: {"7d": (0.10, int(time.time()) + 2 * DAY), "limited": False} for name in maxes}
            self.credit_empty = False
            self.hits.clear()
            self.usage_fail = set()
            self.usage_hits.clear()

    def usage_reads(self):
        """Usage reads so far per login; every read must be the documented request."""
        counts = {}
        with self.lock:
            for name, path, beta in self.usage_hits:
                if path != USAGE_PATH or beta != USAGE_BETA:
                    raise AssertionError(f"usage read of {name}: GET {path} anthropic-beta {beta!r}")
                counts[name] = counts.get(name, 0) + 1
        return counts


class Fixture:
    """One proxy over the shared fake upstream with two OAuth Max logins and a credit key."""

    def __init__(self, binary, run, upstream, ca, seed=None, drained=False, wind_down=None, gate_lines="", credit=True):
        self.binary, self.run, self.upstream, self.ca = binary, run, upstream, ca
        run.mkdir(parents=True)
        (run / "auth").mkdir()
        (run / "home").mkdir()
        upstream.reset(["maxa", "maxb"])
        for name in ("maxa", "maxb"):
            (run / "auth" / f"{name}.json").write_text(json.dumps({
                "type": "claude", "email": f"{name}@example.invalid",
                "access_token": f"sk-ant-oat01-fake-{name}", "refresh_token": f"sk-ant-ort01-fake-{name}",
                "expired": "2099-01-01T00:00:00Z", "last_refresh": "2026-11-07T00:00:00Z",
                "account_uuid": "00000000-0000-4000-8000-0000000000" + ("0a" if name == "maxa" else "0b")}))
            os.chmod(run / "auth" / f"{name}.json", 0o600)
        if seed:
            self.seed(seed)
        self.port = free_port()
        drained_entry = f"""    - name: drained-fake
      base-url: "{upstream.base}/drained"
      priority: 10
      keys:
        - api-key: "drained-upstream-fake"
      models:
        - name: "{MODEL}"
""" if drained else ""
        wind = (f"    wind-down-minutes: {wind_down}\n" if wind_down is not None else "") + gate_lines
        credit_entry = f"""    - name: credit-fake
      base-url: "{upstream.base}/credit"
      priority: -10
      fallback-when: limit
      keys:
        - api-key: "credit-upstream-fake"
      models:
        - name: "{MODEL}"
""" if credit else ""
        keys = f"api-keys:\n  claude:\n{drained_entry}{credit_entry}" if drained or credit else ""
        self.config = run / "config.yaml"
        self.config.write_text(f"""config-version: 8
server:
  host: "127.0.0.1"
  port: {self.port}
access:
  api-keys: [winddown-user-fake, winddown-automation-fake]
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
    exempt-api-keys: [winddown-automation-fake]
{wind}{keys}""")
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.logs = []
        self.start()

    def seed(self, readings):
        """Write the gate's state file: name -> (utilization, reset)."""
        now = time.time()
        stamp = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now))
        accounts = {}
        for name, (usage, reset) in readings.items():
            window = {"utilization": usage, "reset_at": reset, "observed_at": stamp}
            if usage >= 0.95:
                window.update(held=True, held_until=reset)
            accounts[f"{name}.json"] = {"7d": window}
            self.upstream.max[name]["7d"] = (usage, reset)
        path = self.run / "auth" / "reserve-gate.state"
        path.write_text(json.dumps({"version": 1, "updated_at": stamp, "accounts": accounts}))
        os.chmod(path, 0o600)

    def start(self):
        log_path = self.run / f"proxy-{len(self.logs) + 1}.log"
        self.logs.append(log_path)
        self.log = log_path.open("wb")
        env = {"HOME": str(self.run / "home"), "PATH": "/usr/bin:/bin", "SSL_CERT_FILE": str(self.ca)}
        self.process = subprocess.Popen([str(self.binary), "--config", str(self.config)], cwd=self.run, env=env,
                                        stdout=self.log, stderr=subprocess.STDOUT)
        for _ in range(300):
            if self.process.poll() is not None:
                raise RuntimeError(f"proxy exited {self.process.returncode}: see {log_path}")
            try:
                with self.opener.open(f"http://127.0.0.1:{self.port}/", timeout=1):
                    break
            except (OSError, urllib.error.URLError):
                time.sleep(0.1)
        else:
            raise RuntimeError("fixture proxy did not start")
        # The auth files are loaded by the watcher; wait for it like smoke.sh does.
        for _ in range(100):
            if "file watcher started" in log_path.read_text(errors="replace"):
                break
            time.sleep(0.1)
        time.sleep(1)

    def stop(self):
        if self.process.poll() is None:
            self.process.terminate()
            self.process.wait(timeout=15)
        self.log.close()

    def log_text(self):
        self.log.flush()
        return "".join(path.read_text(errors="replace") for path in self.logs)

    def quota_state(self, want):
        """The quota state file once want(document) holds. It must be private and name no
        login by token, email or file name."""
        path = self.run / "home" / ".local" / "state" / "cliproxyapi" / "quota-state.json"
        text, doc = "", None
        for _ in range(50):
            try:
                text = path.read_text()
                doc = json.loads(text)
            except (OSError, ValueError):
                doc = None
            if doc is not None and want(doc):
                break
            time.sleep(0.1)
        else:
            raise AssertionError(f"quota state file never matched: {text[:1200]}")
        mode = path.stat().st_mode & 0o777
        leaks = [word for word in ("fake", "example.invalid", "maxa", "maxb", "sk-ant") if word in text]
        if doc.get("schema") != "cliproxy.quota_state.v1" or mode != 0o600 or leaks:
            raise AssertionError(f"quota state file: schema {doc.get('schema')}, mode {oct(mode)}, leaks {leaks}")
        return doc

    def request(self, body, automation=False, path="/v1/messages"):
        key = "winddown-automation-fake" if automation else "winddown-user-fake"
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}", method="POST",
                                     headers={"Authorization": f"Bearer {key}", "content-type": "application/json",
                                              "anthropic-version": "2023-06-01"},
                                     data=json.dumps(body).encode())
        try:
            response = self.opener.open(req, timeout=60)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.read().decode(), response.headers

    def check(self, name, body, expected_status, expected_routes, automation=False, reserve=False, note=None,
              path="/v1/messages"):
        """expected_routes: exact list, or a set of routes that must all be absent.
        note: True/False checks the wrap-up note at the end of the last upstream user message."""
        before = len(self.upstream.hits)
        status, text, headers = self.request(body, automation, path)
        hits = self.upstream.hits[before:]
        routes = [route for route, _ in hits]
        if status == 429:  # keep every refusal as the client received it
            with (self.run / "refusals.jsonl").open("a") as out:
                out.write(json.dumps({"check": name, "status": status, "headers": dict(headers.items()), "body": text}) + "\n")
        ok = status == expected_status if expected_status else status != 200
        if isinstance(expected_routes, set):
            ok = ok and not expected_routes.intersection(routes)
        else:
            ok = ok and routes == expected_routes
        if reserve:
            ok = ok and "quota reserve hold" in text.lower()
        detail = ""
        if note is not None and ok:
            payload = hits[-1][1]
            got, detail = note_at_end(payload)
            ok = got == note
        if not ok:
            raise AssertionError(f"{name}: HTTP {status} routes {routes}, expected {expected_status} {expected_routes}"
                                 f" note={note} {detail}: {text[:400]}")
        retry = headers.get("Retry-After")
        extra = f" Retry-After={retry}" if retry else ""
        if note is not None:
            extra += f" note={'at end of last user message' if note else 'absent'}" + (f" ({detail})" if detail else "")
        print(f"PASS {name}: HTTP {status} via {routes}{extra}", flush=True)
        return status, text, headers


def note_at_end(payload):
    """Whether the note is the last block of the last user message, after the tool results,
    and nowhere in the system prompt."""
    messages = payload.get("messages") or []
    if not messages or messages[-1].get("role") != "user":
        return False, "last message not a user message"
    content = messages[-1].get("content")
    if not isinstance(content, list) or not content:
        return False, "last user message has no blocks"
    system = payload.get("system")
    in_system = NOTE in json.dumps(system) if system is not None else False
    occurrences = json.dumps(payload).count("Quota reserve reached.")
    last = content[-1]
    at_end = last.get("type") == "text" and last.get("text") == NOTE
    types = [block.get("type") for block in content]
    if at_end and (in_system or occurrences != 1 or "tool_result" not in types[:-1]):
        return False, f"blocks {types}, in system {in_system}, occurrences {occurrences}"
    if not at_end and occurrences:
        return False, f"note present but not last: blocks {types}"
    return at_end, f"blocks {types}" if at_end else ""


def both_held_wind_down_restart(binary, run, upstream, ca, minutes=1):
    """(a) both logins held: new prompt 429, running task served by a login with the note at
    the end of the last user message, OpenAI-format running task too, exempt served.
    (b) after the wind-down (minutes, default 1): running task 429, exempt served; with more than
    1 minute, a running task one minute before the end is still served with the note.
    (f) restart: holds come back from the state file, the wind-down clock restarts."""
    now = int(time.time())
    started = time.time()
    f = Fixture(binary, run, upstream, ca, seed={"maxa": (0.98, now + 2 * DAY), "maxb": (0.99, now + 3 * DAY)},
                wind_down=minutes)
    try:
        f.check("(a) both held: new prompt 429 reserve hold, no upstream call", NEW_PROMPT, 429, [], reserve=True)
        f.check("(a) both held: running task 200 from a Max login, note appended", RUNNING_TASK, 200, {"credit", "drained"},
                note=True)
        routes = [route for route, _ in upstream.hits]
        if not routes or routes[-1] not in ("maxa", "maxb"):
            raise AssertionError(f"running task not served by a Max login: {routes}")
        f.check("(a) both held: running task streamed, note appended", dict(RUNNING_TASK, stream=True), 200,
                {"credit", "drained"}, note=True)
        f.check("(a) both held: OpenAI-format running task (last role tool) 200, note appended",
                OPENAI_RUNNING_TASK, 200, {"credit", "drained"}, note=True, path="/v1/chat/completions")
        f.check("(a) both held: OpenAI-format new prompt 429", {"model": MODEL, "max_tokens": 16,
                "messages": [{"role": "user", "content": "start"}]}, 429, [], path="/v1/chat/completions")
        f.check("(a) both held: exempt new prompt 200 from a Max login, no note", NEW_PROMPT, 200, {"credit", "drained"},
                automation=True, note=False)
        f.check("(a) both held: exempt running task 200, no note", RUNNING_TASK, 200, {"credit"}, automation=True, note=False)
        started_line = [line for line in f.log_text().splitlines() if "running tasks are served until" in line]
        if len(started_line) != 1:
            raise AssertionError(f"want one wind-down start log line, got {started_line}")
        print("PASS (a) wind-down start logged:", started_line[0].split("reserve gate:")[1].strip(), flush=True)
        if minutes > 1:
            wait = started + (minutes - 1) * 60 - time.time()
            print(f"... waiting {wait:.0f}s until one minute before the {minutes} minute wind-down ends", flush=True)
            time.sleep(max(wait, 0))
            f.check(f"(b) {minutes - 1} minutes in: running task still 200, note appended", RUNNING_TASK, 200,
                    {"credit", "drained"}, note=True)
        wait = started + minutes * 60 + 2 - time.time()
        if wait > 0:
            print(f"... waiting {wait:.0f}s for the {minutes} minute wind-down to pass", flush=True)
            time.sleep(wait)
        f.check("(b) after the wind-down: running task 429 reserve hold, no upstream call", RUNNING_TASK, 429, [], reserve=True)
        f.check("(b) after the wind-down: exempt 200", RUNNING_TASK, 200, {"credit"}, automation=True, note=False)
        f.stop()
        f.start()
        f.check("(f) after restart: new prompt 429 from the state file, no upstream call", NEW_PROMPT, 429, [], reserve=True)
        f.check("(f) after restart: running task 200 again (in-memory clock restarted), note appended", RUNNING_TASK, 200,
                {"credit"}, note=True)
    finally:
        f.stop()


def limited_and_held(binary, run, upstream, ca):
    """(c2) maxa at a real limit, maxb held: new prompt 429, no credit; running task served by
    maxb with the note."""
    now = int(time.time())
    f = Fixture(binary, run, upstream, ca, seed={"maxb": (0.96, now + 3 * DAY)})
    try:
        upstream.max["maxa"].update({"7d": (1.0, now + 2 * DAY), "limited": True})
        f.check("(c2) record maxa's real limit with an exempt request", NEW_PROMPT, 200, {"credit"}, automation=True)
        f.check("(c2) maxa limited, maxb held: new prompt 429, no credit", NEW_PROMPT, 429, [], reserve=True)
        f.check("(c2) maxa limited, maxb held: running task 200 by maxb, note appended", RUNNING_TASK, 200, ["maxb"], note=True)
    finally:
        f.stop()


def both_limited(binary, run, upstream, ca):
    """(c) both logins at a real limit: running task served by the credit key, no note; new
    prompt served by it too (its own fallback-when rule). Then an empty credit balance is
    refused and the key cools down."""
    now = int(time.time())
    f = Fixture(binary, run, upstream, ca)
    try:
        upstream.max["maxa"].update({"7d": (1.0, now + 2 * DAY), "limited": True})
        upstream.max["maxb"].update({"7d": (1.0, now + 3 * DAY), "limited": True})
        status, _, _ = f.request(NEW_PROMPT, automation=True)
        routes = [route for route, _ in upstream.hits]
        if status != 200 or routes[-1:] != ["credit"] or not {"maxa", "maxb"} <= set(routes):
            raise AssertionError(f"exempt request did not record both limits: {status} {routes}")
        print(f"PASS (c) exempt request recorded both real limits via {routes}", flush=True)
        f.check("(c) both limited: running task 200 by the credit key, no note", RUNNING_TASK, 200, ["credit"], note=False)
        f.check("(c) both limited: new prompt 200 by the credit key", NEW_PROMPT, 200, ["credit"], note=False)
        upstream.credit_empty = True
        f.check("(c) empty credit balance refused", RUNNING_TASK, None, ["credit"])
        f.check("(c) empty credit key cools down", RUNNING_TASK, None, {"credit"})
    finally:
        f.stop()


def drained_key(binary, run, upstream, ca):
    """(d) a credit key the drain moved to priority 10 without fallback-when, and both logins
    held: it is not a Max login with room, so a new prompt is still refused; a running task is
    served by it (first in priority) with the note."""
    now = int(time.time())
    f = Fixture(binary, run, upstream, ca, seed={"maxa": (0.98, now + 2 * DAY), "maxb": (0.99, now + 3 * DAY)},
                drained=True)
    try:
        f.check("(d) drained key + both held: new prompt 429 reserve hold, no upstream call", NEW_PROMPT, 429, [],
                reserve=True)
        f.check("(d) drained key + both held: running task 200 by the drained key, note appended", RUNNING_TASK, 200,
                ["drained"], note=True)
        f.check("(d) drained key + both held: exempt 200 by the drained key, no note", NEW_PROMPT, 200, ["drained"],
                automation=True, note=False)
    finally:
        f.stop()


def trigger_off(binary, run, upstream, ca):
    """(e) maxa held, maxb at 30%: the trigger is off; new prompt and running task are served
    by Max logins without the note."""
    now = int(time.time())
    f = Fixture(binary, run, upstream, ca, seed={"maxa": (0.97, now + 2 * HOUR), "maxb": (0.30, now + 6 * DAY)})
    try:
        f.check("(e) one login with room: new prompt 200, no note", NEW_PROMPT, 200, {"credit"}, note=False)
        f.check("(e) one login with room: running task 200, no note", RUNNING_TASK, 200, {"credit"}, note=False)
        routes = [route for route, _ in upstream.hits]
        if not set(routes) <= {"maxa", "maxb"}:
            raise AssertionError(f"served by {routes}, want Max logins")
        if "running tasks are served until" in f.log_text():
            raise AssertionError("wind-down started while a login had room")
        print("PASS (e) no wind-down started", flush=True)
    finally:
        f.stop()


def near_reset_limits(binary, run, upstream, ca):
    """(g) one shared reserve: 5% while the next reset of any login is more than 6 h away, 1%
    from 6 h to 1 h, nothing in the final hour, placed on the login that resets last."""
    now = int(time.time())
    f = Fixture(binary, run / "pool-room", upstream, ca, seed={"maxa": (0.98, now + 9 * HOUR), "maxb": (0.96, now + 26 * HOUR)})
    try:
        f.check("(g) maxa 98% (9 h), maxb 96% (26 h): 6% left is above the 5% reserve, new prompt 200", NEW_PROMPT, 200,
                {"credit"})
    finally:
        f.stop()
    f = Fixture(binary, run / "pool-held", upstream, ca, seed={"maxa": (0.99, now + 9 * HOUR), "maxb": (0.96, now + 26 * HOUR)})
    try:
        _, _, headers = f.check("(g) maxa 99%, maxb 96%: 5% left is the reserve, new prompt 429", NEW_PROMPT, 429, [],
                                reserve=True)
        retry = int(headers.get("Retry-After", "0"))
        if abs(retry - 3 * HOUR) > 120:
            raise AssertionError(f"Retry-After {retry}, want about {3 * HOUR} (6 h before maxa's reset)")
        print(f"PASS (g) Retry-After {retry} names the 1% step, 6 h before maxa's reset", flush=True)
        f.check("(g) pool at the reserve: exempt 200 by a Max login", NEW_PROMPT, 200, {"credit"}, automation=True)
    finally:
        f.stop()
    f = Fixture(binary, run / "near-held", upstream, ca, seed={"maxa": (1.0, now + 30 * HOUR), "maxb": (0.99, now + 5 * HOUR)})
    try:
        _, _, headers = f.check("(g) maxb's reset in 5 h: 1% left is the 1% reserve, new prompt 429", NEW_PROMPT, 429, [],
                                reserve=True)
        retry = int(headers.get("Retry-After", "0"))
        if abs(retry - 4 * HOUR) > 120:
            raise AssertionError(f"Retry-After {retry}, want about {4 * HOUR} (maxb's final hour)")
        print(f"PASS (g) Retry-After {retry} names maxb's final hour, 4 h from now", flush=True)
    finally:
        f.stop()
    f = Fixture(binary, run / "final-hour", upstream, ca, seed={"maxa": (1.0, now + 30 * HOUR), "maxb": (0.99, now + 50 * 60)})
    try:
        f.check("(g) maxb's reset in 50 minutes: no reserve, new prompt 200 by a Max login", NEW_PROMPT, 200, {"credit"})
    finally:
        f.stop()
    lines = "    near-reset-hours: 12\n    near-reset-threshold: 0.98\n    final-release-hours: 2\n"
    f = Fixture(binary, run / "explicit-keys", upstream, ca, gate_lines=lines,
                seed={"maxa": (0.99, now + 30 * HOUR), "maxb": (0.98, now + 10 * HOUR)})
    try:
        # With the defaults the 5% reserve would hold both; with a 12 h window at 98% the reserve is 2%.
        f.check("(g) explicit keys: 3% left, next reset in 10 h, reserve 2%: new prompt 200", NEW_PROMPT, 200, {"credit"})
    finally:
        f.stop()


def retry_days(route):
    """Days from now until the route's retry_at; 0 without one."""
    if not route.get("retry_at"):
        return 0
    return (time.mktime(time.strptime(route["retry_at"], "%Y-%m-%dT%H:%M:%SZ")) - time.mktime(time.gmtime())) / DAY


def wait_for_next_usage_read(first_read):
    wait = first_read + USAGE_READ_SECONDS + 1 - time.time()
    if wait > 0:
        print(f"... waiting {wait:.0f}s until the proxy may read usage again", flush=True)
        time.sleep(wait)


def usage_read_before_credit(binary, run, upstream, ca):
    """(h) both logins at a real limit, credit key configured. A failed usage read changes
    nothing; usage is read once per login in 60 s; once the fake upstream reports maxa as
    reset, the next read lifts its cooldown and maxa serves again with no manual step."""
    now = int(time.time())
    f = Fixture(binary, run, upstream, ca)
    try:
        upstream.max["maxa"].update({"7d": (1.0, now + 2 * DAY), "limited": True})
        upstream.max["maxb"].update({"7d": (1.0, now + 3 * DAY), "limited": True})
        upstream.usage_fail = {"maxa", "maxb"}
        status, _, _ = f.request(NEW_PROMPT, automation=True)
        first_read = time.time()
        routes = [route for route, _ in upstream.hits]
        if status != 200 or routes[-1:] != ["credit"] or not {"maxa", "maxb"} <= set(routes):
            raise AssertionError(f"exempt request did not record both limits: {status} {routes}")
        if upstream.usage_reads() != {"maxa": 1, "maxb": 1}:
            raise AssertionError(f"usage reads before the credit fallback: {upstream.usage_reads()}")
        print(f"PASS (h) both limits recorded via {routes}; usage read once per login before the credit key served,"
              " both reads failed (HTTP 500)", flush=True)
        f.check("(h) failed usage read changes nothing: new prompt 200 by the credit key", NEW_PROMPT, 200, ["credit"])
        doc = f.quota_state(lambda d: d["interactive"]["route"] == "credit")
        states = sorted(login["state"] for login in doc["logins"])
        if states != ["cooled", "cooled"] or doc["interactive"]["limit"] != "weekly_all" or retry_days(doc["interactive"]) < 1:
            raise AssertionError(f"quota state after the failed read: {doc}")
        print(f"PASS (h) quota state file: interactive route credit, limit weekly_all, both logins cooled,"
              f" retry in {retry_days(doc['interactive']):.1f} days", flush=True)

        # The owner resets maxa on Anthropic's side: the upstream serves it and reports 2%.
        upstream.max["maxa"] = {"7d": (0.02, now + 7 * DAY), "limited": False}
        upstream.usage_fail = set()
        f.check("(h) maxa reset upstream, under 60 s since the last read: still the credit key", NEW_PROMPT, 200, ["credit"])
        if upstream.usage_reads() != {"maxa": 1, "maxb": 1}:
            raise AssertionError(f"usage read again within {USAGE_READ_SECONDS} s: {upstream.usage_reads()}")
        print(f"PASS (h) no second usage read within {USAGE_READ_SECONDS} s: {upstream.usage_reads()}", flush=True)
        wait_for_next_usage_read(first_read)
        doc = f.quota_state(lambda d: d["interactive"]["route"] == "credit")
        if retry_days(doc["interactive"]) < 1:
            raise AssertionError(f"cooldown ended by itself, the scenario proves nothing: {doc['interactive']}")
        f.check("(h) after 60 s: usage read lifts maxa's cooldown, new prompt 200 by maxa, no manual step",
                NEW_PROMPT, 200, ["maxa"])
        if upstream.usage_reads() != {"maxa": 2, "maxb": 2}:
            raise AssertionError(f"usage reads after the limit passed: {upstream.usage_reads()}")
        f.check("(h) normal path: new prompt 200 by maxa", NEW_PROMPT, 200, ["maxa"])
        if upstream.usage_reads() != {"maxa": 2, "maxb": 2}:
            raise AssertionError(f"usage read on the normal path: {upstream.usage_reads()}")
        doc = f.quota_state(lambda d: d["interactive"]["route"] == "max")
        states = sorted(login["state"] for login in doc["logins"])
        if states != ["cooled", "ok"] or doc["interactive"]["limit"] or doc["interactive"]["message"]:
            raise AssertionError(f"quota state after the recovery: {doc}")
        print(f"PASS (h) no usage read on the normal path: {upstream.usage_reads()}; quota state file: interactive"
              " route max, one login ok, one cooled", flush=True)
    finally:
        f.stop()


def usage_read_before_refusal(binary, run, upstream, ca):
    """(i) both logins at a real limit, no credit key. The refusal names the limit and the
    reset in the text and the headers; once the fake upstream reports maxb as reset, the
    read before the next refusal lifts its cooldown and maxb serves."""
    now = int(time.time())
    f = Fixture(binary, run, upstream, ca, credit=False)
    try:
        upstream.max["maxa"].update({"7d": (1.0, now + 2 * DAY), "limited": True})
        upstream.max["maxb"].update({"7d": (1.0, now + 3 * DAY), "limited": True})
        status, _, _ = f.request(NEW_PROMPT)
        routes = [route for route, _ in upstream.hits]
        if status != 429 or set(routes) != {"maxa", "maxb"}:
            raise AssertionError(f"first request did not record both limits: {status} {routes}")
        print(f"PASS (i) both limits recorded via {routes}: the upstream's own 429 is passed on", flush=True)
        _, text, headers = f.check("(i) both limited, no credit key: new prompt 429, no upstream call", NEW_PROMPT, 429, [])
        first_read = time.time()
        if upstream.usage_reads() != {"maxa": 1, "maxb": 1}:
            raise AssertionError(f"usage reads before the refusal: {upstream.usage_reads()}")
        print("PASS (i) usage read once per login before the proxy's refusal, both still full", flush=True)
        countdown = RESET_AFTER.search(text)
        reset = int(headers.get("Anthropic-Ratelimit-Unified-Reset", "0"))
        retry = int(headers.get("Retry-After", "0"))
        if ("Weekly limit. Next reset " not in text or "A Fable reset does not lift it." not in text or not countdown
                or countdown.group(1) not in ("47", "48") or abs(reset - (now + 2 * DAY)) > 120 or abs(retry - 2 * DAY) > 120
                or headers.get("Anthropic-Ratelimit-Unified-Status") != "rejected"
                or headers.get("Anthropic-Ratelimit-Unified-Representative-Claim") != "seven_day"):
            raise AssertionError(f"refusal does not name the limit and maxa's reset: {text[:400]} {dict(headers)}")
        sentence = text[text.index("Weekly limit."):text.index(" (reset after")]
        print(f"PASS (i) refusal text: {sentence} {countdown.group(0)}; Retry-After {retry},"
              f" unified reset {reset - now} s from now, claim seven_day", flush=True)
        doc = f.quota_state(lambda d: d["interactive"]["route"] == "refused")
        if doc["interactive"]["message"] != sentence or doc["interactive"]["limit"] != "weekly_all":
            raise AssertionError(f"quota state message differs from the refusal: {doc['interactive']} vs {sentence!r}")
        print("PASS (i) quota state file: interactive route refused, limit weekly_all, the refusal's own sentence", flush=True)

        upstream.max["maxb"] = {"7d": (0.05, now + 7 * DAY), "limited": False}
        f.check("(i) maxb reset upstream, under 60 s since the last read: still 429", NEW_PROMPT, 429, [])
        if upstream.usage_reads() != {"maxa": 1, "maxb": 1}:
            raise AssertionError(f"usage read again within {USAGE_READ_SECONDS} s: {upstream.usage_reads()}")
        print(f"PASS (i) no second usage read within {USAGE_READ_SECONDS} s: {upstream.usage_reads()}", flush=True)
        wait_for_next_usage_read(first_read)
        f.check("(i) after 60 s: usage read lifts maxb's cooldown, new prompt 200 by maxb, no manual step",
                NEW_PROMPT, 200, ["maxb"])
        if upstream.usage_reads() != {"maxa": 2, "maxb": 2}:
            raise AssertionError(f"usage reads after the limit passed: {upstream.usage_reads()}")
        f.quota_state(lambda d: d["interactive"]["route"] == "max")
        print(f"PASS (i) usage reads {upstream.usage_reads()}; quota state file: interactive route max", flush=True)
    finally:
        f.stop()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=pathlib.Path, required=True)
    parser.add_argument("--run-dir", type=pathlib.Path, required=True)
    parser.add_argument("--wind-down-minutes", type=int, default=1)
    args = parser.parse_args()
    run = args.run_dir.resolve()
    run.mkdir(parents=True, exist_ok=False)
    binary = args.binary.resolve()
    if socket.gethostbyname("api.anthropic.com") != "127.0.0.1":
        raise SystemExit("api.anthropic.com must resolve to 127.0.0.1 (run through winddown_smoke.sh)")
    ca, cert, key = make_ca(run / "ca")
    upstream = Upstream((ca, cert, key))
    print(f"binary {binary}", flush=True)
    both_held_wind_down_restart(binary, run / "a-b-f-both-held", upstream, ca, args.wind_down_minutes)
    limited_and_held(binary, run / "c2-limited-held", upstream, ca)
    both_limited(binary, run / "c-both-limited", upstream, ca)
    drained_key(binary, run / "d-drained-key", upstream, ca)
    trigger_off(binary, run / "e-trigger-off", upstream, ca)
    near_reset_limits(binary, run / "g-near-reset", upstream, ca)
    usage_read_before_credit(binary, run / "h-usage-credit", upstream, ca)
    usage_read_before_refusal(binary, run / "i-usage-refusal", upstream, ca)
    print("ALL PASS: reserve wind-down through real HTTP with fake OAuth logins", flush=True)


if __name__ == "__main__":
    main()

"""Exercise the real official Linux binary against two local fake providers.

GATEWAY_BIN points to a compiled Linux CLI. No external provider credentials.
"""
import contextlib
import http.server
import json
import os
import pathlib
import shutil
import signal
import sqlite3
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

BIN = os.environ.get("GATEWAY_BIN", "/test/new-api-gateway")
received = []


class Provider(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        raw = self.rfile.read(int(self.headers["Content-Length"]))
        received.append((self.server.label, self.path, raw, self.headers.get("Authorization")))
        if self.server.fail:
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"error":{"message":"mock unavailable","type":"server_error"}}')
            return
        body = json.loads(raw)
        response = {"id": "resp_mock", "object": "response", "created_at": int(time.time()),
                    "status": "completed", "model": body["model"],
                    "output": [{"id": "msg_mock", "type": "message", "role": "assistant", "status": "completed",
                                "content": [{"type": "output_text", "text": "hello", "annotations": []}]}],
                    "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
        if body.get("stream"):
            events = [
                ("response.created", {"type": "response.created", "response": {**response, "status": "in_progress", "output": []}}),
                ("response.output_text.delta", {"type": "response.output_text.delta", "item_id": "msg_mock", "output_index": 0, "content_index": 0, "delta": "hello"}),
                ("response.completed", {"type": "response.completed", "response": response}),
            ]
            data = "".join(f"event: {event}\ndata: {json.dumps(value)}\n\n" for event, value in events).encode()
            mime = "text/event-stream"
        else:
            data = json.dumps(response).encode()
            mime = "application/json"
        self.send_response(200)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def call(port, path, data=None, key=None):
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    request = urllib.request.Request(f"http://127.0.0.1:{port}{path}", data=data, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def command(command, config, state, success=True):
    result = subprocess.run([BIN, command, "--config", str(config), "--state", str(state)], capture_output=True, timeout=600)
    if success and result.returncode:
        # CLI errors are redacted; no upstream log or credential file is printed.
        raise AssertionError(result.stderr.decode())
    return result


def start(config, state, log):
    process = subprocess.Popen([BIN, "run", "--config", str(config), "--state", str(state)], stdout=log, stderr=log)
    for _ in range(600):
        if process.poll() is not None:
            raise AssertionError("run exited; inspect private test logs")
        try:
            if json.loads(call(3000, "/api/setup")[1])["data"]["status"] and (state / "client.json").exists():
                # Upstream starts listening 100ms before registering SIGTERM.
                # Wait through that documented startup window before testing shutdown.
                time.sleep(0.25)
                return process
        except (OSError, ValueError, KeyError):
            pass
        time.sleep(0.1)
    process.kill()
    process.wait()
    raise AssertionError("readiness timeout")


def stop(process):
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=40)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
            raise AssertionError("SIGTERM did not stop New API")
    assert process.returncode == 0


def check_database(state):
    with sqlite3.connect(state / "data/new-api.db") as db:
        assert db.execute("PRAGMA journal_mode").fetchone()[0] == "wal"
        rows = db.execute("SELECT id,name,status,setting,tag FROM channels ORDER BY id").fetchall()
        assert len(rows) == 2
        assert all(json.loads(row[3])["pass_through_body_enabled"] for row in rows)
        assert all(not row[4] for row in rows)
        options = dict(db.execute("SELECT key,value FROM options"))
        assert options["LogConsumeEnabled"] == "false"
        assert options["RegisterEnabled"] == "false"
        assert options["performance_setting.monitor_enabled"] == "false"
        assert json.loads(options["GroupRatio"])["default"] == 0
        assert db.execute("SELECT COUNT(*) FROM logs WHERE type=2").fetchone()[0] == 0
        return rows, db.execute("SELECT quota FROM users WHERE username='gateway'").fetchone()[0]


def main():
    servers = []
    for label in ["primary", "backup"]:
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Provider)
        server.label, server.fail = label, False
        threading.Thread(target=server.serve_forever, daemon=True).start()
        servers.append(server)
    process = None
    # Set KEEP_TEST_STATE only for a failing local investigation. Never upload it.
    temp = pathlib.Path(tempfile.mkdtemp(prefix="new-api-integration-"))
    try:
        config, state = temp / "gateway.json", temp / "state"
        value = {"version": 1, "port": 3000, "retries": 2, "channels": [
            {"id": label, "name": label.capitalize(), "type": "openai",
             "base_url": f"http://127.0.0.1:{server.server_port}", "key": "test-key-" + label,
             "models": ["gpt-5.4", "gpt-6-astra"], "priority": 10 if i == 0 else 0}
            for i, (label, server) in enumerate(zip(["primary", "backup"], servers))]}
        def save():
            config.write_text(json.dumps(value))
            config.chmod(0o600)
        save()
        command("validate", config, state)
        cache = os.environ.get("TEST_RELEASE_CACHE")
        if cache and pathlib.Path(cache).exists():
            (state / "bin").mkdir(parents=True, mode=0o700)
            for binary in pathlib.Path(cache).glob("new-api-*"):
                shutil.copy2(binary, state / "bin" / binary.name)
        # A real download, SHA-256 and ELF machine verification.
        command("install", config, state)
        if cache:
            pathlib.Path(cache).mkdir(parents=True, exist_ok=True)
            for binary in (state / "bin").glob("new-api-*"):
                shutil.copy2(binary, pathlib.Path(cache) / binary.name)
        with (temp / "run.log").open("wb") as log:
            process = start(config, state, log)
            key = json.loads((state / "client.json").read_text())["api_key"]
            identity = (state / "state.json").read_bytes()
            dbrows, quota_before = check_database(state)
            status = json.loads(call(3000, "/api/status")[1])["data"]
            assert status["enable_drawing"] is False and status["enable_task"] is False
            assert call(3000, "/v1/models")[0] == 401
            assert call(3000, "/v1/models", key=key)[0] == 200
            assert json.loads(call(3000, "/api/user/register", b'{}')[1])["success"] is False
            body = b'{ "model": "gpt-5.4", "input": "hello", "store": false, "opaque_extension": {"sealed": "test"} }\n'
            status_code, response = call(3000, "/v1/responses", body, key)
            assert status_code == 200, f"Responses returned {status_code}"
            assert received[-1] == ("primary", "/v1/responses", body, "Bearer test-key-primary")
            assert json.loads(response)["id"] == "resp_mock"
            received.clear()
            servers[0].fail = True
            assert call(3000, "/v1/responses", body, key)[0] == 200
            assert [r[0] for r in received] == ["primary", "backup"], "priority failover failed"
            servers[0].fail = False
            stream = b'{"model":"gpt-5.4","input":"hello","stream":true}'
            code, response = call(3000, "/v1/responses", stream, key)
            assert code == 200 and b"response.completed" in response and b"hello" in response
            # Also exercise a model with built-in tiered billing.
            assert call(3000, "/v1/responses", b'{"model":"gpt-6-astra","input":"hello"}', key)[0] == 200
            _, quota_after = check_database(state)
            assert quota_after == quota_before, "local wallet was charged"
            # Single permanent process and real all-interface listener; no helper child.
            assert "new-api-" in os.readlink(f"/proc/{process.pid}/exe")
            assert b"TASK_PLUGIN_ENABLED=false" in pathlib.Path(f"/proc/{process.pid}/environ").read_bytes()
            assert not pathlib.Path(f"/proc/{process.pid}/task/{process.pid}/children").read_text().strip()
            listeners = pathlib.Path("/proc/net/tcp").read_text() + pathlib.Path("/proc/net/tcp6").read_text()
            assert "00000000000000000000000000000000:0BB8" in listeners or "00000000:0BB8" in listeners
            assert command("init", config, state, success=False).returncode != 0, "serve lock not inherited"
            # Apply twice, rotate a key and priority, no duplicates, same client token.
            value["channels"][1]["priority"] = 20
            value["channels"][1]["key"] = "rotated-test-key"
            save()
            command("apply", config, state)
            command("apply", config, state)
            assert call(3000, "/v1/responses", body, key)[0] == 200
            assert received[-1][0] == "backup" and received[-1][3] == "Bearer rotated-test-key"
            assert check_database(state)[0][0][:2] == dbrows[0][:2]
            # Removed managed channels are disabled, never deleted.
            value["channels"].pop(0)
            save()
            command("apply", config, state)
            assert check_database(state)[0][0][2] == 2
            stop(process)
            process = start(config, state, log)
            assert (state / "state.json").read_bytes() == identity
            assert json.loads((state / "client.json").read_text())["api_key"] == key
            assert call(3000, "/v1/responses", body, key)[0] == 200
            stop(process)
            process = None
            backup = temp / "backup"
            shutil.copytree(state, backup)
            restored = temp / "restored"
            shutil.copytree(backup, restored)
            process = start(config, restored, log)
            assert json.loads((restored / "client.json").read_text())["api_key"] == key
            assert call(3000, "/v1/responses", body, key)[0] == 200
            stop(process)
            process = None
        for path in [state, state / "data", state / "runtime", state / "logs"]:
            assert path.stat().st_mode & 0o777 == 0o700
        for path in [config, state / "state.json", state / "client.json", state / "data/new-api.db"]:
            assert path.stat().st_mode & 0o077 == 0
        logs = (temp / "run.log").read_text() + (state / "logs/bootstrap.log").read_text()
        assert "rotated-test-key" not in logs and "test-key-primary" not in logs and key not in logs
        print("PASS: official Linux install, setup, lean options, native Responses/SSE, raw body passthrough,")
        print("priority/failover, zero local charges, JSON rotation/removal/idempotence, single process,")
        print("serve lock, SIGTERM/restart, SQLite WAL, permissions, backup/restore and no key output")
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            with contextlib.suppress(subprocess.TimeoutExpired):
                process.wait(timeout=40)
            if process.poll() is None:
                process.kill()
                process.wait()
        for server in servers:
            server.shutdown()
            server.server_close()
        if os.environ.get("KEEP_TEST_STATE"):
            print("Private synthetic test state retained at", temp)
        else:
            shutil.rmtree(temp)


if __name__ == "__main__":
    main()

"""Verify supported chat protocols against a running proxy, without saving bodies.

TEST_URL defaults to http://127.0.0.1:8001; ADMIN_TOKEN_FILE defaults to
 data/admin-token. Creates and always revokes one temporary client key.
"""
import json
import os
from pathlib import Path
import time
import urllib.request

base = os.environ.get("TEST_URL", "http://127.0.0.1:8001").rstrip("/")
admin = Path(os.environ.get("ADMIN_TOKEN_FILE", "data/admin-token")).read_text().strip()


def request(path, key, body=None, anthropic=False):
    headers = {"Content-Type": "application/json"}
    if anthropic:
        headers.update({"x-api-key": key, "anthropic-version": "2023-06-01"})
    else:
        headers["Authorization"] = "Bearer " + key
    req = urllib.request.Request(base + path, headers=headers,
                                 data=json.dumps(body).encode() if body is not None else None)
    return urllib.request.urlopen(req, timeout=180)


def api(path, body=None):
    with request(path, admin, body) as response:
        return json.load(response)


created = api("/admin/keys", {"name": "Protocol verification"})
try:
    with request("/v1/models", created["key"]) as response:
        model = json.load(response)["data"][0]["id"]
    from urllib.parse import quote
    with request("/v1/models/" + quote(model, safe=""), created["key"], anthropic=True) as response:
        assert json.load(response)["id"] == model
    for path in ["/v1/messages", "/v1/responses", "/v1/chat/completions", "/v1/completions"]:
        for stream in [False, True]:
            anthropic = path == "/v1/messages"
            body = {"model": model, "stream": stream}
            if path == "/v1/responses":
                body.update({"input": "Say hi.", "max_output_tokens": 4})
            elif path == "/v1/completions":
                body.update({"prompt": "Hello", "max_tokens": 4})
            else:
                body.update({"messages": [{"role": "user", "content": "Say hi."}], "max_tokens": 4})
            usage = {}
            def inspect(value):
                if isinstance(value.get("response"), dict):
                    inspect(value["response"])
                if value.get("type") == "message_start":
                    inspect(value["message"])
                if value.get("usage"):
                    usage.update(value["usage"])
            with request(path, created["key"], body, anthropic) as response:
                assert response.status == 200
                if stream:
                    for line in response:
                        if line.startswith(b"data:"):
                            payload = line[5:].strip()
                            if payload and payload != b"[DONE]":
                                inspect(json.loads(payload))
                else:
                    inspect(json.load(response))
            input_tokens = usage.get("prompt_tokens", usage.get("input_tokens"))
            output_tokens = usage.get("completion_tokens", usage.get("output_tokens"))
            assert input_tokens is not None and output_tokens is not None, "upstream usage missing"
            if anthropic:
                input_tokens += usage.get("cache_read_input_tokens", 0) + usage.get("cache_creation_input_tokens", 0)
            time.sleep(.4)
            rows = api("/admin/requests?key=" + str(created["id"]))
            record = rows[0]
            assert record["path"] == path and record["status"] == 200
            assert record["input_tokens"] == input_tokens
            assert record["output_tokens"] == output_tokens
            assert record["outcome"] == "complete"
            print(json.dumps({"path": path, "stream": stream, "input": input_tokens,
                              "output": output_tokens, "cached": record["cached_tokens"],
                              "duration_ms": record["duration_ms"], "accounting": "matched"}), flush=True)
    print("All four inference protocols passed in JSON and streaming modes.", flush=True)
finally:
    api("/admin/revoke", {"id": created["id"]})

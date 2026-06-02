#!/usr/bin/env python3
"""Mock Weave Router for the @workweave/router end-to-end test.

Speaks just enough of the Anthropic Messages API to drive a real pi process
headlessly, with no real model spend and no network beyond localhost:

  GET  /health, /validate    -> 200            (the install.sh --pi probes)
  POST <any path>            -> Anthropic Messages response (SSE or JSON)

It also *is* the model. To exercise the `dispatch` tool it returns a tool_use
block for `dispatch` when the latest user turn contains DISPATCH_MARKER and no
tool_result is present yet; once the tool_result comes back it answers with
plain text so the agent loop terminates. Subagent calls (X-App: pi-subagent)
always get plain text. Every response carries an `x-router-model` header so the
extension's routed-model path fires.

Every request is appended as one JSON object per line to MOCK_LOG so the e2e
script can assert the header / knob / metadata.user_id shape. The router key is
never logged in full -- only presence + last 4 chars.

Env:
  MOCK_PORT          listen port                       (default 8899)
  MOCK_LOG           request log path (JSONL)          (default ./requests.jsonl)
  MOCK_MAIN_MODEL    x-router-model for main requests  (default claude-opus-4-8)
  MOCK_SUBAGENT_MODEL x-router-model for subagents     (default claude-haiku-4-5)
  DISPATCH_MARKER    main-loop prompt trigger          (default __DISPATCH__)
"""

import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("MOCK_PORT", "8899"))
LOG_PATH = os.environ.get("MOCK_LOG", os.path.join(os.getcwd(), "requests.jsonl"))
MAIN_MODEL = os.environ.get("MOCK_MAIN_MODEL", "claude-opus-4-8")
SUBAGENT_MODEL = os.environ.get("MOCK_SUBAGENT_MODEL", "claude-haiku-4-5")
DISPATCH_MARKER = os.environ.get("DISPATCH_MARKER", "__DISPATCH__")

# The real router serves the Anthropic Messages API at exactly this path. We
# reject anything else with 404 so a wrong baseUrl (e.g. a doubled /v1 from the
# Anthropic SDK appending /v1/messages to a baseUrl that already ends in /v1)
# fails the test instead of being silently absorbed by a catch-all.
MESSAGES_PATH = "/v1/messages"

KNOB_HEADERS = (
    "x-weave-routing-alpha",
    "x-weave-routing-speed-weight",
    "x-weave-routing-output-cost-ratio",
    "x-weave-routing-expected-output-tokens",
)
DISPATCH_TASKS = [
    {"prompt": "Reply with exactly: SUBAGENT_ONE_OK"},
    {"prompt": "Reply with exactly: SUBAGENT_TWO_OK"},
]

_log_lock = threading.Lock()


def log_request(record: dict) -> None:
    line = json.dumps(record, separators=(",", ":"))
    with _log_lock:
        with open(LOG_PATH, "a", encoding="utf-8") as fh:
            fh.write(line + "\n")
    print(
        f"[mock] {record['method']} {record['path']} "
        f"app={record.get('app')} user_id={record.get('user_id')} "
        f"served={record.get('served', '-')}",
        file=sys.stderr,
        flush=True,
    )


def latest_user_text(messages: list) -> str:
    for msg in reversed(messages):
        if not isinstance(msg, dict) or msg.get("role") != "user":
            continue
        content = msg.get("content")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            parts = [
                b.get("text", "")
                for b in content
                if isinstance(b, dict) and b.get("type") == "text"
            ]
            return " ".join(parts)
        return ""
    return ""


def has_tool_result(messages: list) -> bool:
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        content = msg.get("content")
        if isinstance(content, list):
            for block in content:
                if isinstance(block, dict) and block.get("type") == "tool_result":
                    return True
    return False


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args) -> None:  # silence the default access log
        return

    # ---- helpers -------------------------------------------------------

    def _send_json(self, code: int, obj: dict, extra_headers: dict | None = None) -> None:
        payload = json.dumps(obj).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        for key, val in (extra_headers or {}).items():
            self.send_header(key, val)
        self.end_headers()
        try:
            self.wfile.write(payload)
        except BrokenPipeError:
            pass

    def _send_sse(self, events: list, routed_model: str) -> None:
        body = "".join(
            f"event: {ev}\ndata: {json.dumps(data)}\n\n" for ev, data in events
        ).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("x-router-model", routed_model)
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass

    # ---- response builders --------------------------------------------

    @staticmethod
    def _message_obj(block: dict, stop_reason: str, routed_model: str) -> dict:
        return {
            "id": "msg_mock",
            "type": "message",
            "role": "assistant",
            "model": routed_model,
            "content": [block],
            "stop_reason": stop_reason,
            "stop_sequence": None,
            "usage": {"input_tokens": 12, "output_tokens": 8},
        }

    @staticmethod
    def _sse_events(block: dict, stop_reason: str, routed_model: str) -> list:
        start_msg = {
            "id": "msg_mock",
            "type": "message",
            "role": "assistant",
            "model": routed_model,
            "content": [],
            "stop_reason": None,
            "stop_sequence": None,
            "usage": {"input_tokens": 12, "output_tokens": 1},
        }
        events = [("message_start", {"type": "message_start", "message": start_msg})]
        if block["type"] == "text":
            events += [
                (
                    "content_block_start",
                    {
                        "type": "content_block_start",
                        "index": 0,
                        "content_block": {"type": "text", "text": ""},
                    },
                ),
                (
                    "content_block_delta",
                    {
                        "type": "content_block_delta",
                        "index": 0,
                        "delta": {"type": "text_delta", "text": block["text"]},
                    },
                ),
            ]
        else:  # tool_use
            events += [
                (
                    "content_block_start",
                    {
                        "type": "content_block_start",
                        "index": 0,
                        "content_block": {
                            "type": "tool_use",
                            "id": block["id"],
                            "name": block["name"],
                            "input": {},
                        },
                    },
                ),
                (
                    "content_block_delta",
                    {
                        "type": "content_block_delta",
                        "index": 0,
                        "delta": {
                            "type": "input_json_delta",
                            "partial_json": json.dumps(block["input"]),
                        },
                    },
                ),
            ]
        events += [
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            (
                "message_delta",
                {
                    "type": "message_delta",
                    "delta": {"stop_reason": stop_reason, "stop_sequence": None},
                    "usage": {"output_tokens": 8},
                },
            ),
            ("message_stop", {"type": "message_stop"}),
        ]
        return events

    # ---- handlers ------------------------------------------------------

    def do_GET(self) -> None:  # noqa: N802 (stdlib naming)
        path = self.path.split("?")[0]
        log_request({"method": "GET", "path": path, "app": self.headers.get("x-app")})
        self._send_json(200, {"status": "ok"})

    def do_POST(self) -> None:  # noqa: N802 (stdlib naming)
        length = int(self.headers.get("content-length") or 0)
        raw = self.rfile.read(length) if length else b""  # always drain the body
        path = self.path.split("?")[0]

        if path != MESSAGES_PATH:
            log_request(
                {"method": "POST", "path": path, "app": self.headers.get("x-app"), "rejected": True}
            )
            self._send_json(
                404,
                {"type": "error", "error": {"type": "not_found_error", "message": f"no route for POST {path}"}},
            )
            return

        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {}

        messages = body.get("messages") or []
        app = self.headers.get("x-app") or "pi"
        is_subagent = app == "pi-subagent"
        key = self.headers.get("x-weave-router-key") or ""
        metadata = body.get("metadata") or {}
        user_text = latest_user_text(messages)
        tool_result_present = has_tool_result(messages)
        stream = bool(body.get("stream"))

        want_dispatch = (
            DISPATCH_MARKER in user_text and not is_subagent and not tool_result_present
        )
        routed_model = SUBAGENT_MODEL if is_subagent else MAIN_MODEL

        if want_dispatch:
            block = {
                "type": "tool_use",
                "id": "toolu_mock_dispatch",
                "name": "dispatch",
                "input": {"tasks": DISPATCH_TASKS},
            }
            stop_reason = "tool_use"
            served = "tool_use"
        else:
            if is_subagent:
                reply = "SUBAGENT_OK"
            elif tool_result_present:
                reply = "DISPATCH_COMPLETE_OK"
            else:
                reply = "MAIN_LOOP_OK"
            block = {"type": "text", "text": reply}
            stop_reason = "end_turn"
            served = "text"

        log_request(
            {
                "method": "POST",
                "path": path,
                "rejected": False,
                "app": app,
                "model": body.get("model"),
                "stream": stream,
                "user_id": metadata.get("user_id"),
                "key_present": bool(key),
                "key_suffix": key[-4:] if key else "",
                "email": self.headers.get("x-weave-user-email"),
                "name": self.headers.get("x-weave-user-name"),
                "knobs": {h: self.headers.get(h) for h in KNOB_HEADERS},
                "marker_opt": self.headers.get("x-weave-routing-marker"),
                "has_tool_result": tool_result_present,
                "user_text": user_text[:60],
                "served": served,
            }
        )

        if stream:
            self._send_sse(self._sse_events(block, stop_reason, routed_model), routed_model)
        else:
            self._send_json(
                200,
                self._message_obj(block, stop_reason, routed_model),
                extra_headers={"x-router-model": routed_model},
            )


def main() -> None:
    open(LOG_PATH, "w", encoding="utf-8").close()  # truncate per run
    server = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(
        f"[mock] listening on http://127.0.0.1:{PORT}  log={LOG_PATH}",
        file=sys.stderr,
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()

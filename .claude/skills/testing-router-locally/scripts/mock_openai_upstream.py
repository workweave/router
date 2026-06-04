#!/usr/bin/env python3
"""Mock OpenAI-compatible streaming upstream for local router testing.

Serves :8099 and replies to any POST with a fixed SSE stream. Point a provider's
<PROVIDER>_BASE_URL at http://host.docker.internal:8099/v1 so router translation
runs against a deterministic upstream shape — no real API key or credits needed.

Default stream reproduces the GLM-5.1 degenerate tool-call: a tool_calls delta
with an empty function.name, closed with finish_reason="tool_calls". The router
suppresses the nameless call (stream.go), which historically (mis)fired the
text-only recovery nudge. Edit CHUNKS to reproduce a different shape.
"""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8099
BASE = {"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": "mock-model"}

# Each entry is a `choices[0].delta` plus optional finish_reason/usage. Edit to
# reproduce the upstream behavior under test.
CHUNKS = [
    {"delta": {"role": "assistant"}},
    {"delta": {"tool_calls": [
        {"index": 0, "id": "call_x", "type": "function",
         "function": {"name": "", "arguments": ""}}]}},
    {"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "{}"}}]}},
    {"delta": {}, "finish_reason": "tool_calls",
     "usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}},
]


def sse(choice):
    obj = dict(BASE)
    usage = choice.pop("usage", None)
    obj["choices"] = [{"index": 0, "delta": choice.get("delta", {}),
                       "finish_reason": choice.get("finish_reason")}]
    if usage is not None:
        obj["usage"] = usage
    return ("data: " + json.dumps(obj) + "\n\n").encode()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers.get("content-length", 0)))
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.end_headers()
        for chunk in CHUNKS:
            self.wfile.write(sse(dict(chunk)))
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


if __name__ == "__main__":
    print(f"mock OpenAI-compat upstream on :{PORT}")
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()

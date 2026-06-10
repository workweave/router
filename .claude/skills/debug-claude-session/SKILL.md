---
name: debug-claude-session
description: Investigate a specific Claude Code session by session ID — correlate the local transcript (`~/.claude/projects/...jsonl`) with the router's production logs to understand what the client rendered vs. what the upstream served. Use when given a session ID and asked "why did X render?" for a Claude Code conversation routed through the router.
---

# Debugging a Claude Code session

Given a Claude Code **session ID**, pull the local transcript (what the client saw) and the corresponding production cloud logs (which model/provider served it), then correlate them to understand the wire-format translation. The local `.jsonl` is ground truth for *what rendered*; the cloud logs confirm *what the upstream sent*; the `internal/translate` code explains *why the wire shape looks that way*.

## Setup: Cloud deployment config

Before starting, create a gitignored config file with your deployment's cloud logging details:

```bash
cat > .claude/skills/debug-claude-session/.deployment.json <<'EOF'
{
  "cloud_provider": "gcp",
  "project_id": "your-project-id",
  "region": "us-central1",
  "service_name": "router",
  "log_command_template": "gcloud logging read ... --project {project_id} --format=json"
}
EOF
git add .claude/skills/debug-claude-session/.deployment.json.example
# .deployment.json itself should be gitignored
```

If `.deployment.json` is missing, the agent will prompt you for these details and walk you through creating it. The file is gitignored and contains no secrets — it's just the service/project/region names needed to construct cloud log queries.

## Critical gotchas (read first)

- **The transcript is the source of truth for rendering.** What the client showed is exactly the assistant `message.content` blocks in the `.jsonl` — not what you assume the model emitted. Always inspect block contents (decode fields, check lengths) before concluding something is "empty" or "corrupt".
- **Streaming splits one logical turn across multiple `assistant` lines.** Each line may hold a single block; they share a `message.id` and `message.model`. Reconstruct the full turn by collecting all lines with the same `message.id`.
- **Block content may carry encoded state.** Fields like `signature`, `id`, and `input` often encode provider-specific state (e.g. encrypted reasoning). Decode and inspect before skipping or dismissing a block.
- **Cloud logs are structured, not free text.** Filter queries on specific JSON fields (e.g. `jsonPayload.decision_model`, `jsonPayload.message`). The exact field names depend on the router's logging schema — ask if unsure.
- **Correlate by time + model, not request id.** The local transcript has UTC timestamps (`timestamp` field); request IDs are often absent. Use a tight UTC window around transcript timestamps plus the served `decision_model` to find matching cloud log entries.

## Workflow

```
- [ ] 1. Locate the local transcript
- [ ] 2. Extract the assistant blocks showing the symptom
- [ ] 3. Decode block internals (signatures, ids, sizes)
- [ ] 4. Identify model + provider from the transcript
- [ ] 5. Fetch cloud logs for the matching UTC window
- [ ] 6. Correlate transcript + cloud logs
- [ ] 7. Trace to the translation code in internal/translate
```

### 1. Locate the local transcript

```bash
find ~/.claude/projects -name '<SESSION_ID>*' -type f
```

You get `<path>/<SESSION_ID>.jsonl` (the transcript, one JSON object per line) and a sibling `<SESSION_ID>/` directory (tool-output spillover). The `.jsonl` file is the ground truth. `wc -l` it — typical sessions are tens to hundreds of lines.

### 2. Extract the assistant blocks showing the symptom

Each line is a typed event. Scan for `type: "assistant"` entries. Each carries:
- `message.id` — groups lines from the same logical turn.
- `message.model` — the served model (e.g. `gpt-5.5`, `claude-opus-4-8`).
- `message.stop_reason` — how the turn ended (`end_turn`, `tool_use`, `max_tokens`).
- `message.content[]` — list of blocks (`text`, `thinking`, `tool_use`, `tool_result`).

Adapt this template to search for your symptom (empty blocks, missing content, unexpected stop_reason, etc.):

```bash
python3 - <<'EOF'
import json
with open("<path>/<SESSION_ID>.jsonl") as f:
    for i, line in enumerate(f):
        try:
            o = json.loads(line)
        except:
            continue
        if o.get("type") != "assistant":
            continue
        msg = o.get("message", {})
        # Adapt this filter to your symptom:
        for block in msg.get("content", []):
            if isinstance(block, dict) and block.get("type") == "thinking":
                thinking_text = block.get("thinking", "")
                signature = block.get("signature", "")
                if thinking_text == "":  # Your condition here
                    print(f"line {i+1}: model={msg.get('model')} "
                          f"stop_reason={msg.get('stop_reason')} "
                          f"thinking_len={len(thinking_text)} "
                          f"signature_len={len(signature)}")
EOF
```

Print `model`, `stop_reason`, block type/length — enough to see the pattern at a glance.

### 3. Decode block internals

Don't assume a short or empty field is meaningless. Many blocks carry encoded provider state. Inspect the actual bytes:

```bash
python3 - <<'EOF'
import json, base64
line_num = <LINE>  # From step 2
with open("<path>/<SESSION_ID>.jsonl") as f:
    o = json.loads(f.readlines()[line_num - 1])
for block in o["message"].get("content", []):
    block_type = block.get("type")
    print(f"=== {block_type} ===")
    # Print all fields and their lengths:
    for key, val in block.items():
        if isinstance(val, str):
            print(f"  {key}: len={len(val)} head={val[:80]!r}")
        else:
            print(f"  {key}: {type(val).__name__} {val!r}")
    # If any field looks base64-encoded, try decoding:
    if "signature" in block and block["signature"]:
        try:
            decoded = base64.b64decode(block["signature"])
            print(f"  signature (decoded): {decoded[:160]!r}")
        except Exception as e:
            print(f"  signature (decode failed): {e}")
EOF
```

This reveals what's actually inside. Look for:
- Encrypted state (e.g. `encrypted_content`, `enc` fields) that must round-trip to the upstream.
- Embedded IDs (e.g. OpenAI reasoning signatures embedded in `tool_use.id`).
- Redundant carriers — the same state might be duplicated across blocks.

### 4. Identify model + provider from the transcript

From the `message.model` field (step 2), note the served model. This tells you:
- **Which upstream** (e.g. `gpt-*` → OpenAI, `claude-*` → Anthropic, `gemini-*` → Google).
- **Which translation code** handles it (e.g. `gpt-*` Responses → `internal/translate/responses_to_anthropic_writer.go`).

Also note `message.id` (ids are unique per response; the prefix marks the path):
- `msg_responses_*` → OpenAI Responses API path (streaming).
- `msg_translated_*` → OpenAI chat-completions path (router-generated id; upstream-provided ids pass through unchanged).
- `msg_01...` (Anthropic-native id) → Anthropic Messages path (passthrough).

### 5. Fetch cloud logs for the matching UTC window

Extract the timestamp range from the transcript:

```bash
python3 - <<'EOF'
import json
with open("<path>/<SESSION_ID>.jsonl") as f:
    lines = [json.loads(line) for line in f if json.loads(line).get("timestamp")]
    if lines:
        print(f"UTC window: {lines[0]['timestamp']} to {lines[-1]['timestamp']}")
EOF
```

Then use your cloud logging tool with the config from `.deployment.json`:

```bash
# Example for gcloud/GCP. Adapt to your cloud provider:
gcloud logging read \
  'resource.type="cloud_run_revision" AND resource.labels.service_name="router" AND timestamp>="2026-06-10T01:02:00Z" AND timestamp<="2026-06-10T01:08:00Z"' \
  --project <project_id> --limit 20 --format=json \
  > /tmp/cloud_logs.json
```

Filter for the served model to narrow results:

```bash
python3 - <<'EOF'
import json
with open("/tmp/cloud_logs.json") as f:
    for entry in json.load(f):
        payload = entry.get("jsonPayload", {})
        # Adapt filter to your log schema:
        if payload.get("decision_model") == "gpt-5.5":
            print(json.dumps({
                "timestamp": entry.get("timestamp"),
                "message": payload.get("message"),
                "decision_model": payload.get("decision_model"),
                "decision_provider": payload.get("decision_provider"),
                "stop_reason": payload.get("resp_stop_reason")
            }))
EOF
```

### 6. Correlate transcript + cloud logs

Match entries from steps 2 and 5 by:
- **Timestamp** (transcript `timestamp` ≈ cloud log `timestamp` within ~1-2 seconds).
- **Model** (transcript `message.model` == cloud log `decision_model`).

Once matched, the cloud log entry tells you:
- **Which model/provider actually served** the turn (proof that the transcript came from that upstream).
- **How the turn was reconciled** (e.g. `stop_reason` demotions, tool_use handling, usage accounting).

### 7. Trace to the translation code

Now that you've identified the model/provider path (step 4) and confirmed it in cloud logs (step 6), open the relevant translation file in `internal/translate/`:

- `responses_to_anthropic_writer.go` — OpenAI Responses API.
- `stream.go` + `emit_anthropic.go` — OpenAI-compatible chat.
- `gemini_stream.go` + `emit_gemini.go` — Google Generative Language.
- `emit_anthropic.go` — Anthropic passthrough (mostly copy).

Find the emitter function that produces the block shape you're seeing:
- `emitContentBlockStartThinking`, `emitContentBlockDeltaThinking` — thinking blocks.
- `emitContentBlockStartTool`, `emitContentBlockDeltaTool` — tool_use blocks.
- Similar for text, tool_result, etc.

Read backward from the emitter to the upstream event that triggered it. This is where the "why" lives.

## Example: Empty thinking block

1. Local transcript shows: `thinking: ""` but `signature: "<1500 chars>"` (step 3).
2. Cloud log shows: `decision_model=gpt-5.5, decision_provider=openai, stop_reason=tool_use` (step 5-6).
3. Translation path: OpenAI Responses → `responses_to_anthropic_writer.go` (step 4).
4. Trace upstream event: `response.reasoning_summary_text.delta` with empty delta but populated item on `response.output_item.added` (step 7).
5. Emitter: `handleReasoningDelta` → `emitContentBlockDeltaThinking("", ...)` which does nothing (empty delta skipped), but the thinking block was already opened by `handleOutputItemAdded` (line 279).
6. Root cause: OpenAI returned a reasoning item with `encrypted_content` but under `reasoning.summary:"auto"`, no summary text. The block can't be skipped because the signature must round-trip to the next turn.

## Notes

- The transcript records the **post-translation** Anthropic Messages shape — it never shows raw upstream Responses/Gemini events. Reconstruct upstream behavior by reading the emitter code + cloud logs together.
- `message.id` groups lines from the same logical turn; reconstruct the full turn by collecting all lines sharing an `id`.
- If the cloud log query returns nothing or the wrong model, expand the UTC window or check the model name spelling.
- To reproduce a translation artifact locally (without prod), use the sibling `test-claude-locally` skill to run the router with a mock upstream emitting the exact wire shape you're investigating.

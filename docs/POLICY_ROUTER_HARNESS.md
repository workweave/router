# Policy router harness

This is the production contract for plugging an out-of-process routing policy
into Weave Router. A new policy should require a sidecar deployment and one
configuration entry, not strategy-specific proxy, provider, UI, or telemetry
code.

## Ownership boundary

The Go router owns:

- the authoritative set of deployed and request-eligible models;
- model exclusions, provider exclusions, context limits, tools, and images;
- preferred-model ranks and quality/price inputs;
- the exact provider and upstream binding for each candidate;
- upstream dispatch, credentials, retries, billing, and telemetry;
- managed service-to-service authentication for policy sidecar calls;
- installation rollout state, debug authorization, and training eligibility;
- validation that the sidecar selected one offered roster ID and provider.

The sidecar owns only policy inference and policy-specific state. It must not
dispatch the selected completion itself. Managed policy candidate lists never
contain OpenRouter bindings. A production sidecar must not send prompts,
outcomes, or feedback to OpenRouter for auxiliary inference either; use the
same direct provider stack approved for the router deployment.

There is no strategy fallback. If a serving policy cannot return a valid
selection after bounded transient retries, the client receives HTTP 503.
Availability comes from healthy replicas, readiness gates, immutable artifacts,
and staged rollout rather than a second hidden policy.

## Required HTTP surface

Every sidecar exposes:

| Endpoint | Contract |
| -------- | -------- |
| `GET /livez` | Process liveness only. |
| `GET /readyz` | The exact artifact and roster are loaded and routing can succeed. |
| `GET /capabilities` | Returns the `policy_router_v1` optional-feature declaration. |
| `POST /route` | Selects exactly one candidate supplied by the router. |
| `POST /outcome` | Accepts final dispatch status, usage, latency, and policy provenance. |
| `POST /feedback` | Accepts explicit user/session feedback. |

If outcome or feedback handling is intentionally a no-op, return 2xx and set
the corresponding capability to `false`. The router will then stop sending
that optional callback after capability discovery.

Example capabilities response:

```json
{
  "schema_version": "policy_router_v1",
  "reports_outcomes": true,
  "reports_feedback": true,
  "honors_preferred_models": true,
  "honors_quality_price_bias": true,
  "supports_debug_route_detail": true,
  "supports_preview": false,
  "supports_shadow": true,
  "authoritative_per_turn_selection": false
}
```

When `authoritative_per_turn_selection=true`, each eligible main-loop or
tool-result `/route` selection is model-authoritative for that turn. Go still
owns hard eligibility, explicit force-model/operator pins, credentials, and
same-model provider retries. It disables automatic session reuse, EV planner
overrides, model-changing baseline failover, semantic-cache hits, and
router-generated summarizer calls for those turns. Post-selection synthetic
loop breakers are also bypassed so one accepted policy action maps to one
selected model dispatch attempt.

## Route contract

The router sends a stable `route_id`, strategy, execution mode, organization
and installation identifiers, client harness, bounded conversation context,
tools, routing intent, preferred models, product knobs, privacy state, and a
structured candidate list. Candidate bindings are authoritative.

```json
{
  "schema_version": "policy_router_v1",
  "strategy": "quality-v2",
  "execution_mode": "serving",
  "route_id": "2f93b729-8ae2-4d23-889a-d7b59c729790",
  "client_app": "codex",
  "rollout_id": "quality-v2-prod-1",
  "routing_intent": "high",
  "preferred_models": ["gpt-5.5"],
  "visible_turn_index": 7,
  "session_turn_count": 9,
  "turn_type": "tool_result",
  "previous_served_model": "claude-opus-4-8",
  "previous_provider": "anthropic",
  "cache_state": "warm",
  "prior_output_tokens": 321,
  "session_ever_switched": true,
  "history_truncated": false,
  "training_allowed": false,
  "debug_enabled": false,
  "candidates": [
    {
      "roster_id": "gpt-5.5",
      "catalog_id": "gpt-5.5",
      "provider": "openai",
      "upstream_id": "gpt-5.5",
      "preference_rank": 0,
      "input_usd_per_1m": 5.0,
      "output_usd_per_1m": 30.0,
      "estimated_cost_usd": 0.03,
      "cache_read_multiplier": 0.1,
      "marginal_cost_factor": 0.25,
      "effective_input_usd_per_1m": 1.25,
      "effective_output_usd_per_1m": 7.5,
      "effective_estimated_cost_usd": 0.0075,
      "capabilities": {
        "context_window": 400000,
        "tier": "high",
        "supports_tools": true,
        "supports_images": true
      }
    }
  ]
}
```

Return the offered `roster_id`. `selected_provider` may be omitted; if present,
it must exactly match the candidate binding. The generic `policy_route_key`
holds any policy-internal arm, bucket, cluster, or mode. During migration,
`routing_bucket` is accepted as an alias.

```json
{
  "schema_version": "policy_router_v1",
  "route_id": "2f93b729-8ae2-4d23-889a-d7b59c729790",
  "selected_roster_id": "gpt-5.5",
  "selected_provider": "openai",
  "score": 0.91,
  "candidate_scores": {"gpt-5.5": 0.91},
  "propensity": 1.0,
  "policy_route_key": "high",
  "policy_artifact_id": "quality-v2-prod-1",
  "policy_artifact_sha256": "<64 lowercase hex characters>",
  "roster_version": "roster-2026-07-09"
}
```

The router rejects empty selections, unknown roster IDs, provider mismatches,
and unsupported schema versions. Rich `debug` data is internal to the sidecar;
only an opaque `debug_ref` is projected when authorized debug mode is enabled.

`POST /outcome` reports `selected_model`, `selected_provider`, `served_model`,
`served_provider`, `selected_served_model_match`, and
`authoritative_per_turn_selection`. An authoritative model mismatch is marked
ineligible for training and logged as an error.

## Privacy and execution modes

`training_allowed` is false unless the organization is currently eligible for
AI training. False means the sidecar must not retain request or response
content, mutate online-learning state, enqueue judges, or publish the event as
training data. Serving still proceeds.

`execution_mode=shadow` means decision-only comparison. The router forces
`training_allowed=false`, suppresses debug detail, never dispatches the shadow
selection, and records only content-free comparison metadata. A sidecar must
apply the same rule independently.

Artifacts trained offline must carry the repository's training-privacy
attestation and be promoted by immutable ID plus SHA-256. Production must not
resolve `latest`, local files, or an unverified mutable URL.

## Add a policy

1. Implement policy inference behind the standard endpoints. Python sidecars
   should import `ml_dev.policy_router.contract` for candidate parsing,
   execution-mode validation, harness normalization, and fail-closed privacy.
2. Add contract tests for candidate validation, unknown schema versions,
   provider echoing, shadow non-learning behavior, debug gating, outcomes, and
   feedback. Test that opted-out traffic produces no learning artifact.
3. Package the exact model, roster, feature contract, and privacy attestation;
   promote it through the normal bake-off gate and pin its ID and SHA-256.
4. Deploy at least two production replicas with startup, readiness, and
   liveness probes. Confirm a fresh replica loads the pinned artifact before it
   becomes ready.
   Managed Cloud Run sidecars must require an identity token whose audience is
   the exact sidecar origin and grant invocation only to the router identity.
5. Add the sidecar origin to `ROUTER_POLICY_SIDECARS`, restart the router, and
   verify `GET /v1/router/policies` reports the strategy and capabilities.
6. Exercise serving and shadow traffic for every supported harness. Verify
   model/provider/artifact/roster/route-key/rollout telemetry and lifecycle
   callbacks join on `route_id`.
7. Start with installation allowlisting. Keep the deployment default at
   `cluster` until error, latency, provider, privacy, and quality gates pass.
8. Promote globally by changing `ROUTER_DEFAULT_STRATEGY`. Keep an explicit
   installation override available for operational rollback; do not add an
   automatic per-request fallback.

Example registration:

```bash
export ROUTER_POLICY_SIDECARS='{"quality-v2":"https://quality-v2.internal"}'
export ROUTER_POLICY_SIDECAR_AUTH='{"quality-v2":"google-id-token"}'
export ROUTER_POLICY_SIDECAR_TIMEOUT_MS=3000
```

The strategy ID must match `[a-z][a-z0-9_-]{0,63}` and cannot be `cluster`,
`rl`, `hmm`, or `bandit`. New policies use catalog IDs as roster IDs at the
wire boundary; an artifact with different internal labels translates them
inside the sidecar. `google-id-token` authentication uses the exact configured
origin as the token audience and fails router startup when credentials cannot
build an authenticated client.

## Release gates

A policy is ready for production only when all of these are true:

- the bake-off and promotion artifact are recorded and reproducible;
- production references an immutable artifact ID and verified SHA-256;
- roster coverage matches the currently deployed router catalog;
- no managed candidate or auxiliary inference path uses OpenRouter;
- opted-out and shadow requests cannot enter any learning sink;
- sidecar loss produces bounded retries and an observable 503, never fallback;
- at least two ready replicas survive a single-replica loss test;
- managed sidecar invocation is authenticated and restricted to the router identity;
- serving and shadow telemetry include strategy, rollout, route ID, artifact,
  roster, route key, selected model/provider, latency, status, and privacy mode;
- the internal policy catalog and dashboard reflect live capabilities and
  installation rollout state.

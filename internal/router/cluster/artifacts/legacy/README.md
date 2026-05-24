# Legacy v1-format cluster artifacts

Every bundle in this directory is `format_version=1`: the per-(cluster,
model) score in `rankings.json` is the pre-blended scalar, with α,
speed_weight, output_cost_ratio, and verbosity all baked in at training
time.

These are kept around for two reasons:

1. **Eval reproducibility.** Past comparisons in
   `docs/plans/ROUTER_CANDIDATE_RESULTS.md` reference specific
   versions; rerunning a comparison must produce the same numbers.
2. **Dual-version eval.** When deploying with
   `ROUTER_CLUSTER_BUILD_ALL_VERSIONS=true` (staging only), the runtime
   constructs one `Scorer` per committed bundle so the eval harness can
   pin per-request to any of them via `x-weave-cluster-version`.

## Rules

- **Do not modify or rewrite any file here.** Bundles are frozen at the
  byte level; any edit invalidates the eval comparisons that reference
  them.
- New training runs write to the parent `artifacts/` directory in v2
  format — never into `legacy/`.
- The `x-weave-routing-*` knob headers are no-ops against any bundle
  in this directory; v1 lacks the components needed for runtime
  re-blending. Override-driven sweeps must target a v2 bundle.

The `bundleDirForVersion` resolver in `artifacts.go` finds a version in
either the parent `artifacts/` directory or here, so calling
`ResolveVersion("v0.38")` works unchanged after the move.

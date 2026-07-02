-- Returns the org's autopay configuration read by the router's debit hook to
-- detect a balance crossing below the recharge threshold. A no-row result
-- (the org never configured autopay) maps to a not-found error; the caller
-- treats that as "autopay disabled" and skips the crossing check. Only the two
-- columns the router needs are selected -- every other autopay column is
-- written and read exclusively by the Weave control plane.
-- name: GetAutopayConfig :one
SELECT enabled, threshold_usd_micros
FROM router.organization_autopay_config
WHERE organization_id = @organization_id::varchar;

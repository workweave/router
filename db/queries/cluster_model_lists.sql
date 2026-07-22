-- Returns all cluster allowlists for a given API key. Writes are owned by the
-- Weave control plane (direct inserts to this schema, mirroring
-- excluded_models / preferred_models); the router only reads on the auth path.
-- name: GetClusterModelListsByAPIKey :many
SELECT *
FROM router.cluster_model_lists
WHERE api_key_id = @api_key_id::uuid
ORDER BY cluster_label;

package auth

import "context"

// ClusterModelList is a per-API-key, per-cluster ordered model allowlist.
// Models is serving priority order (index 0 = highest). Empty slices are never
// persisted — the DB enforces cardinality > 0.
type ClusterModelList struct {
	APIKeyID     string
	ClusterLabel string
	Models       []string
}

// ClusterModelListRepository reads per-key per-cluster ordered allowlists.
// Writes are control-plane-owned (direct inserts); the router is read-only on
// the auth path.
type ClusterModelListRepository interface {
	// GetForAPIKey returns every configured cluster allowlist for a key.
	GetForAPIKey(ctx context.Context, apiKeyID string) ([]ClusterModelList, error)
}

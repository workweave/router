package postgres

import (
	"context"

	"workweave/router/internal/auth"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
)

type clusterModelListRepo struct {
	tx sqlc.DBTX
}

// NewClusterModelListRepo constructs a per-key per-cluster allowlist repo.
func NewClusterModelListRepo(tx sqlc.DBTX) auth.ClusterModelListRepository {
	return &clusterModelListRepo{tx: tx}
}

func (r *clusterModelListRepo) GetForAPIKey(ctx context.Context, apiKeyID string) ([]auth.ClusterModelList, error) {
	parsed, err := uuid.Parse(apiKeyID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	rows, err := q.GetClusterModelListsByAPIKey(ctx, parsed)
	if err != nil {
		return nil, err
	}
	out := make([]auth.ClusterModelList, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuthClusterModelList(row))
	}
	return out, nil
}

func toAuthClusterModelList(row sqlc.RouterClusterModelList) auth.ClusterModelList {
	return auth.ClusterModelList{
		APIKeyID:     row.APIKeyID.String(),
		ClusterLabel: row.ClusterLabel,
		Models:       row.Models,
	}
}

package postgres

import (
	"context"

	"workweave/router/internal/auth"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type userRepo struct {
	tx sqlc.DBTX
}

// NewUserRepository wires the SQLC user queries to auth.UserRepository.
func NewUserRepository(tx sqlc.DBTX) auth.UserRepository {
	return &userRepo{tx: tx}
}

func (r *userRepo) UpsertByEmail(ctx context.Context, params auth.UpsertUserParams) (*auth.User, error) {
	installationID, err := uuid.Parse(params.InstallationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	row, err := q.UpsertModelRouterUserByEmail(ctx, sqlc.UpsertModelRouterUserByEmailParams{
		InstallationID:    installationID,
		Email:             params.Email,
		ClaudeAccountUUID: claudeAccountUUIDArg(params.ClaudeAccountUUID),
	})
	if err != nil {
		return nil, err
	}
	return toAuthUser(row), nil
}

func (r *userRepo) UpsertByAccountUUID(ctx context.Context, params auth.UpsertUserByAccountUUIDParams) (*auth.User, error) {
	installationID, err := uuid.Parse(params.InstallationID)
	if err != nil {
		return nil, err
	}
	accountUUID, err := uuid.Parse(params.ClaudeAccountUUID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	row, err := q.UpsertModelRouterUserByAccountUUID(ctx, sqlc.UpsertModelRouterUserByAccountUUIDParams{
		InstallationID:    installationID,
		ClaudeAccountUUID: accountUUID,
	})
	if err != nil {
		return nil, err
	}
	return toAuthUser(row), nil
}

func (r *userRepo) Get(ctx context.Context, id string) (*auth.User, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	row, err := q.GetModelRouterUser(ctx, parsed)
	if err != nil {
		return nil, err
	}
	return toAuthUser(row), nil
}

func (r *userRepo) ListForInstallation(ctx context.Context, installationID string) ([]*auth.User, error) {
	parsed, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	rows, err := q.ListModelRouterUsersForInstallation(ctx, parsed)
	if err != nil {
		return nil, err
	}
	out := make([]*auth.User, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuthUser(row))
	}
	return out, nil
}

func toAuthUser(row sqlc.RouterModelRouterUser) *auth.User {
	user := &auth.User{
		ID:             row.ID.String(),
		InstallationID: row.InstallationID.String(),
		FirstSeenAt:    timestampOrZero(row.FirstSeenAt),
		LastSeenAt:     timestampOrZero(row.LastSeenAt),
		DeletedAt:      timestampPtr(row.DeletedAt),
	}
	if row.Email != nil {
		user.Email = *row.Email
	}
	if row.ClaudeAccountUUID.Valid {
		s := uuid.UUID(row.ClaudeAccountUUID.Bytes).String()
		user.ClaudeAccountUUID = &s
	}
	return user
}

func claudeAccountUUIDArg(s *string) pgtype.UUID {
	if s == nil || *s == "" {
		return pgtype.UUID{Valid: false}
	}
	parsed, err := uuid.Parse(*s)
	if err != nil {
		return pgtype.UUID{Valid: false}
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}
}

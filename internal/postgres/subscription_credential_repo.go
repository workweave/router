package postgres

import (
	"context"
	"errors"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/sqlc"
	"workweave/router/internal/subscriptions"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// AAD purpose suffixes bind each ciphertext to its column so an access-token
// ciphertext can't be swapped into the refresh slot (or vice versa), and a
// BYOK ciphertext can never decrypt in a subscription slot.
const (
	aadAccessSuffix  = ":sub:access"
	aadRefreshSuffix = ":sub:refresh"
)

// SubscriptionCredentialRepo implements subscriptions.Repository over SQLC,
// owning encryption at rest (Tink AEAD, same envelope as BYOK keys).
type SubscriptionCredentialRepo struct {
	tx        sqlc.DBTX
	encryptor auth.Encryptor
}

// NewSubscriptionCredentialRepo constructs a SubscriptionCredentialRepo.
func NewSubscriptionCredentialRepo(tx sqlc.DBTX, encryptor auth.Encryptor) *SubscriptionCredentialRepo {
	return &SubscriptionCredentialRepo{tx: tx, encryptor: encryptor}
}

// txBeginner is the pgx transaction capability the pgxpool.Pool provides.
// ReplaceByFingerprint needs it to run the soft-delete + insert atomically;
// the pool passed at construction satisfies it.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

func (r *SubscriptionCredentialRepo) Create(ctx context.Context, params subscriptions.CreateParams) (*subscriptions.Credential, error) {
	installationUUID, err := uuid.Parse(params.InstallationID)
	if err != nil {
		return nil, err
	}
	accessCipher, err := r.encryptor.Encrypt(params.AccessToken, params.ExternalID, params.Provider+aadAccessSuffix)
	if err != nil {
		return nil, err
	}
	refreshCipher, err := r.encryptor.Encrypt(params.RefreshToken, params.ExternalID, params.Provider+aadRefreshSuffix)
	if err != nil {
		return nil, err
	}

	q := sqlc.New(r.tx)
	row, err := q.CreateSubscriptionCredential(ctx, sqlc.CreateSubscriptionCredentialParams{
		InstallationID:         installationUUID,
		ExternalID:             params.ExternalID,
		UserEmail:              params.UserEmail,
		Provider:               params.Provider,
		AccountLabel:           stringPtrOrNil(params.AccountLabel),
		AccountFingerprint:     params.AccountFingerprint,
		ChatgptAccountID:       stringPtrOrNil(params.ChatGPTAccountID),
		AccessTokenCiphertext:  accessCipher,
		RefreshTokenCiphertext: refreshCipher,
		AccessTokenExpiresAt:   timestamptzOrNull(params.ExpiresAt),
		CreatedBy:              stringPtrOrNil(params.CreatedBy),
	})
	if err != nil {
		return nil, err
	}
	return r.toCredential(row)
}

func (r *SubscriptionCredentialRepo) GetActiveForUser(ctx context.Context, installationID, userEmail string) ([]*subscriptions.Credential, error) {
	installationUUID, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	rows, err := q.GetActiveSubscriptionCredentialsForUser(ctx, sqlc.GetActiveSubscriptionCredentialsForUserParams{
		InstallationID: installationUUID,
		UserEmail:      userEmail,
	})
	if err != nil {
		return nil, err
	}
	return r.toCredentials(rows)
}

func (r *SubscriptionCredentialRepo) ListForUser(ctx context.Context, installationID, userEmail string) ([]*subscriptions.Credential, error) {
	installationUUID, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	rows, err := q.ListSubscriptionCredentialsForUser(ctx, sqlc.ListSubscriptionCredentialsForUserParams{
		InstallationID: installationUUID,
		UserEmail:      userEmail,
	})
	if err != nil {
		return nil, err
	}
	return r.toCredentials(rows)
}

func (r *SubscriptionCredentialRepo) UpdateTokens(ctx context.Context, id, externalID, provider string, accessToken, refreshToken []byte, expiresAt time.Time) error {
	credUUID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	accessCipher, err := r.encryptor.Encrypt(accessToken, externalID, provider+aadAccessSuffix)
	if err != nil {
		return err
	}
	refreshCipher, err := r.encryptor.Encrypt(refreshToken, externalID, provider+aadRefreshSuffix)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.UpdateSubscriptionCredentialTokens(ctx, sqlc.UpdateSubscriptionCredentialTokensParams{
		AccessTokenCiphertext:  accessCipher,
		RefreshTokenCiphertext: refreshCipher,
		AccessTokenExpiresAt:   timestamptzOrNull(expiresAt),
		ID:                     credUUID,
	})
}

func (r *SubscriptionCredentialRepo) MarkRefreshFailed(ctx context.Context, id string) error {
	credUUID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.MarkSubscriptionCredentialRefreshFailed(ctx, credUUID)
}

func (r *SubscriptionCredentialRepo) MarkUsed(ctx context.Context, id string) error {
	credUUID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.MarkSubscriptionCredentialUsed(ctx, credUUID)
}

func (r *SubscriptionCredentialRepo) SoftDelete(ctx context.Context, installationID, userEmail, id string) (bool, error) {
	installationUUID, err := uuid.Parse(installationID)
	if err != nil {
		return false, err
	}
	credUUID, err := uuid.Parse(id)
	if err != nil {
		return false, err
	}
	q := sqlc.New(r.tx)
	rows, err := q.SoftDeleteSubscriptionCredential(ctx, sqlc.SoftDeleteSubscriptionCredentialParams{
		ID:             credUUID,
		InstallationID: installationUUID,
		UserEmail:      userEmail,
	})
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// ReplaceByFingerprint soft-deletes any existing row for the account and
// inserts the new credential in a single transaction, so a failed insert
// rolls back the delete and leaves the prior credential intact.
func (r *SubscriptionCredentialRepo) ReplaceByFingerprint(ctx context.Context, params subscriptions.CreateParams) (*subscriptions.Credential, error) {
	installationUUID, err := uuid.Parse(params.InstallationID)
	if err != nil {
		return nil, err
	}
	accessCipher, err := r.encryptor.Encrypt(params.AccessToken, params.ExternalID, params.Provider+aadAccessSuffix)
	if err != nil {
		return nil, err
	}
	refreshCipher, err := r.encryptor.Encrypt(params.RefreshToken, params.ExternalID, params.Provider+aadRefreshSuffix)
	if err != nil {
		return nil, err
	}

	beginner, ok := r.tx.(txBeginner)
	if !ok {
		return nil, errors.New("subscription credential store is not transaction-capable")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := sqlc.New(tx)
	if err := q.SoftDeleteSubscriptionCredentialByFingerprint(ctx, sqlc.SoftDeleteSubscriptionCredentialByFingerprintParams{
		InstallationID:     installationUUID,
		UserEmail:          params.UserEmail,
		Provider:           params.Provider,
		AccountFingerprint: params.AccountFingerprint,
	}); err != nil {
		return nil, err
	}
	row, err := q.CreateSubscriptionCredential(ctx, sqlc.CreateSubscriptionCredentialParams{
		InstallationID:         installationUUID,
		ExternalID:             params.ExternalID,
		UserEmail:              params.UserEmail,
		Provider:               params.Provider,
		AccountLabel:           stringPtrOrNil(params.AccountLabel),
		AccountFingerprint:     params.AccountFingerprint,
		ChatgptAccountID:       stringPtrOrNil(params.ChatGPTAccountID),
		AccessTokenCiphertext:  accessCipher,
		RefreshTokenCiphertext: refreshCipher,
		AccessTokenExpiresAt:   timestamptzOrNull(params.ExpiresAt),
		CreatedBy:              stringPtrOrNil(params.CreatedBy),
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.toCredential(row)
}

func (r *SubscriptionCredentialRepo) toCredentials(rows []sqlc.RouterSubscriptionCredential) ([]*subscriptions.Credential, error) {
	out := make([]*subscriptions.Credential, 0, len(rows))
	for _, row := range rows {
		cred, err := r.toCredential(row)
		if err != nil {
			return nil, err
		}
		out = append(out, cred)
	}
	return out, nil
}

func (r *SubscriptionCredentialRepo) toCredential(row sqlc.RouterSubscriptionCredential) (*subscriptions.Credential, error) {
	access, err := r.encryptor.Decrypt(row.AccessTokenCiphertext, row.ExternalID, row.Provider+aadAccessSuffix)
	if err != nil {
		return nil, err
	}
	refresh, err := r.encryptor.Decrypt(row.RefreshTokenCiphertext, row.ExternalID, row.Provider+aadRefreshSuffix)
	if err != nil {
		return nil, err
	}
	return &subscriptions.Credential{
		ID:               row.ID.String(),
		ExternalID:       row.ExternalID,
		InstallationID:   row.InstallationID.String(),
		UserEmail:        row.UserEmail,
		Provider:         row.Provider,
		AccountLabel:     stringOrEmpty(row.AccountLabel),
		ChatGPTAccountID: stringOrEmpty(row.ChatgptAccountID),
		AccessToken:      access,
		RefreshToken:     refresh,
		ExpiresAt:        timestamptzOrZero(row.AccessTokenExpiresAt),
		LastUsedAt:       timestamptzOrZero(row.LastUsedAt),
		RefreshFailedAt:  timestamptzOrZero(row.RefreshFailedAt),
		CreatedAt:        timestamptzOrZero(row.CreatedAt),
	}, nil
}

func timestamptzOrNull(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

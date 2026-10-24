package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RevokeSession durably records that a signed session id may no longer be used.
// It is idempotent: repeated logout calls refresh the same row. The expiry is
// the session's natural TTL, after which the row is pruned.
func (s *Store) RevokeSession(ctx context.Context, sessionID string, revokedAt, expiresAt time.Time) error {
	if s == nil || s.writeDB == nil {
		return errors.New("storage: closed store")
	}
	if sessionID == "" {
		return errors.New("storage: empty session id")
	}
	if !expiresAt.After(revokedAt) {
		return errors.New("storage: session revocation expiry must be after revocation")
	}
	if _, err := s.writeDB.ExecContext(ctx, `
		INSERT INTO session_revocations (session_id, revoked_at_ms, expires_at_ms)
		VALUES (?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET
			revoked_at_ms = excluded.revoked_at_ms,
			expires_at_ms = MAX(session_revocations.expires_at_ms, excluded.expires_at_ms)`,
		sessionID, revokedAt.UTC().UnixMilli(), expiresAt.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("storage: revoke session: %w", err)
	}
	return nil
}

// SessionRevoked reports whether the session id was explicitly revoked and the
// revocation has not expired. Errors are returned so callers can fail closed.
func (s *Store) SessionRevoked(ctx context.Context, sessionID string, now time.Time) (bool, error) {
	if s == nil || s.readDB == nil {
		return false, errors.New("storage: closed store")
	}
	if sessionID == "" {
		return true, nil
	}
	var expires int64
	err := s.readDB.QueryRowContext(ctx,
		`SELECT expires_at_ms FROM session_revocations WHERE session_id = ?`, sessionID).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: session revocation lookup: %w", err)
	}
	return expires > now.UTC().UnixMilli(), nil
}

// PruneSessionRevocations deletes revocations whose session has already
// expired. It is safe to call at any time and returns the number of removed
// rows.
func (s *Store) PruneSessionRevocations(ctx context.Context, now time.Time) (int64, error) {
	if s == nil || s.writeDB == nil {
		return 0, errors.New("storage: closed store")
	}
	res, err := s.writeDB.ExecContext(ctx,
		`DELETE FROM session_revocations WHERE expires_at_ms <= ?`, now.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("storage: prune session revocations: %w", err)
	}
	return res.RowsAffected()
}

// CountSessionRevocations returns the number of stored revocation rows. It is
// used by operator diagnostics and tests.
func (s *Store) CountSessionRevocations(ctx context.Context) (int64, error) {
	if s == nil || s.readDB == nil {
		return 0, errors.New("storage: closed store")
	}
	var n int64
	if err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_revocations`).Scan(&n); err != nil {
		return 0, fmt.Errorf("storage: count session revocations: %w", err)
	}
	return n, nil
}

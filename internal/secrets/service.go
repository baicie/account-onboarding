// Package secrets implements the encrypted secret store on top of the storage
// package and the AES-256-GCM boundary.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

// ErrInvalidRef is returned for malformed opaque references.
var ErrInvalidRef = errors.New("invalid secret reference")

// Service implements provider.SecretStore.
type Service struct {
	store *storage.Store
	box   *security.Box
}

// New returns a secret service.
func New(store *storage.Store, box *security.Box) *Service {
	return &Service{store: store, box: box}
}

// Ref builds an opaque secret reference.
func Ref(kind, id string) string {
	return "secret://" + kind + "/" + id
}

// ParseRef splits an opaque reference into kind and id.
func ParseRef(ref string) (kind, id string, err error) {
	rest, ok := strings.CutPrefix(ref, "secret://")
	if !ok {
		return "", "", ErrInvalidRef
	}
	kind, id, ok = strings.Cut(rest, "/")
	if !ok || kind == "" || id == "" || strings.ContainsAny(kind+id, " /?#") {
		return "", "", ErrInvalidRef
	}
	return kind, id, nil
}

// PutNew encrypts plaintext under a fresh random id and returns its reference.
func (s *Service) PutNew(ctx context.Context, kind, plaintext string) (string, error) {
	if kind == "" {
		return "", ErrInvalidRef
	}
	id := security.NewID()
	if err := s.Put(ctx, Ref(kind, id), kind, plaintext); err != nil {
		return "", err
	}
	return Ref(kind, id), nil
}

// Put stores plaintext at the given reference. The reference shape is
// secret://<kind>/<id>; the kind must match.
func (s *Service) Put(ctx context.Context, ref, kind, plaintext string) error {
	rKind, rID, err := ParseRef(ref)
	if err != nil {
		return err
	}
	if kind == "" {
		kind = rKind
	}
	if rKind != kind {
		return fmt.Errorf("%w: kind mismatch (%s != %s)", ErrInvalidRef, rKind, kind)
	}
	sealed, err := s.box.Seal(kind, rID, []byte(plaintext))
	if err != nil {
		return err
	}
	return s.store.PutSecret(ctx, rID, kind, sealed.Ciphertext, sealed.Nonce, sealed.KeyVersion)
}

// Resolve decrypts the referenced secret. Failures are fail-closed.
func (s *Service) Resolve(ctx context.Context, ref string) (string, error) {
	kind, id, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	rec, err := s.store.GetSecret(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("secret not found for reference kind %q", kind)
		}
		return "", err
	}
	if rec.Kind != kind {
		return "", errors.New("secret kind mismatch")
	}
	pt, err := s.box.Open(rec.Kind, rec.ID, security.Sealed{
		Ciphertext: rec.Ciphertext, Nonce: rec.Nonce, KeyVersion: rec.KeyVersion,
	})
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// Delete removes the referenced secret.
func (s *Service) Delete(ctx context.Context, ref string) error {
	_, id, err := ParseRef(ref)
	if err != nil {
		return err
	}
	return s.store.DeleteSecret(ctx, id)
}

// SealAuditValue is a helper for tests and re-encryption jobs.
func (s *Service) SealAuditValue(kind, id, value string) (security.Sealed, error) {
	return s.box.SealString(kind, id, value)
}

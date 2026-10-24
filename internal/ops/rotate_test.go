package ops

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
)

func TestRotateSecretsOldToNewVersionIdempotent(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	plain := map[string]string{}
	var refs []string
	for i, text := range []string{
		"rotate-plaintext-alpha",
		"rotate-plaintext-bravo",
		"rotate-plaintext-charlie",
	} {
		ref := seedSecret(t, st, "sandbox_password", text)
		plain[ref] = text
		refs = append(refs, ref)
		_ = i
	}
	key1 := append([]byte(nil), st.MasterKey...)
	key2 := bytes.Repeat([]byte{0x33}, 32)
	ring2, err := security.NewKeyRingWithVersions(key2, map[int][]byte{1: key1, 2: key2})
	if err != nil {
		t.Fatalf("ring2: %v", err)
	}
	keyringPath := writeKeyringEnvelope(t, 2, map[int][]byte{1: key1, 2: key2})
	keyring2, err := security.LoadKeyRing(keyringPath)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	if keyring2.CurrentVersion() != 2 {
		t.Fatalf("keyring current = %d, want 2", keyring2.CurrentVersion())
	}

	before := readSecretRowsForTest(t, st.DBPath)
	if len(before) != len(refs) {
		t.Fatalf("seeded %d secrets, found %d", len(refs), len(before))
	}
	for _, row := range before {
		if row.version != 1 {
			t.Fatalf("seeded secret %s version = %d, want 1", row.id, row.version)
		}
	}

	res, err := RotateSecrets(ctx, RotationOptions{DBPath: st.DBPath, KeyRingPath: keyringPath})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if res.CurrentVersion != 2 || res.TotalSecrets != len(refs) || res.Rotated != len(refs) {
		t.Fatalf("rotation result = %+v", res)
	}
	if res.VersionsBefore[1] != len(refs) {
		t.Fatalf("versions before = %+v", res.VersionsBefore)
	}
	if res.VersionsAfter[2] != len(refs) || len(res.VersionsAfter) != 1 {
		t.Fatalf("versions after = %+v", res.VersionsAfter)
	}
	if res.VerifiedAfter != len(refs) {
		t.Fatalf("verified after = %d, want %d", res.VerifiedAfter, len(refs))
	}

	// All plaintexts must still resolve under the new ring, via the real service.
	newSvc := secrets.New(st.Store, security.NewBox(ring2))
	for ref, want := range plain {
		got, err := newSvc.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("resolve after rotation: %v", err)
		}
		if got != want {
			t.Fatalf("plaintext mismatch after rotation for %s", ref)
		}
	}

	// Idempotency: a second pass changes nothing, byte for byte.
	afterFirst := readSecretRowsForTest(t, st.DBPath)
	res2, err := RotateSecrets(ctx, RotationOptions{DBPath: st.DBPath, KeyRingPath: keyringPath})
	if err != nil {
		t.Fatalf("second rotate: %v", err)
	}
	if res2.Rotated != 0 || res2.AlreadyCurrent != len(refs) {
		t.Fatalf("second rotation = %+v", res2)
	}
	afterSecond := readSecretRowsForTest(t, st.DBPath)
	if len(afterFirst) != len(afterSecond) {
		t.Fatalf("secret count changed between rotations")
	}
	for i := range afterFirst {
		if !bytes.Equal(afterFirst[i].ciphertext, afterSecond[i].ciphertext) ||
			!bytes.Equal(afterFirst[i].nonce, afterSecond[i].nonce) {
			t.Fatalf("idempotent rotation rewrote ciphertext for %s", afterFirst[i].id)
		}
	}
}

func TestRotateSecretsDryRunChangesNothing(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		seedSecret(t, st, "sandbox_password", "dry-run-secret-"+string(rune('a'+i)))
	}
	key1 := append([]byte(nil), st.MasterKey...)
	key2 := bytes.Repeat([]byte{0x44}, 32)
	keyringPath := writeKeyringEnvelope(t, 2, map[int][]byte{1: key1, 2: key2})
	before := readSecretRowsForTest(t, st.DBPath)

	res, err := RotateSecrets(ctx, RotationOptions{DBPath: st.DBPath, KeyRingPath: keyringPath, DryRun: true})
	if err != nil {
		t.Fatalf("dry-run rotate: %v", err)
	}
	if !res.DryRun || res.Rotated != 0 || res.Planned != len(before) {
		t.Fatalf("dry-run result = %+v", res)
	}
	after := readSecretRowsForTest(t, st.DBPath)
	for i := range before {
		if before[i].version != after[i].version ||
			!bytes.Equal(before[i].ciphertext, after[i].ciphertext) ||
			!bytes.Equal(before[i].nonce, after[i].nonce) {
			t.Fatalf("dry-run modified row %s", before[i].id)
		}
	}
}

func TestRotateSecretsMissingKeyVersionFailsWithoutDataLoss(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	ref := seedSecret(t, st, "sandbox_password", "missing-version-secret")
	before := readSecretRowsForTest(t, st.DBPath)

	// Ring current=2 but without version 1: rotation cannot read existing rows.
	key2 := bytes.Repeat([]byte{0x55}, 32)
	keyringPath := writeKeyringEnvelope(t, 2, map[int][]byte{2: key2})
	_, err := RotateSecrets(ctx, RotationOptions{DBPath: st.DBPath, KeyRingPath: keyringPath})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("missing-version error = %v", err)
	}
	after := readSecretRowsForTest(t, st.DBPath)
	if len(after) != len(before) {
		t.Fatalf("row count changed on failed rotation")
	}
	for i := range before {
		if before[i].version != after[i].version ||
			!bytes.Equal(before[i].ciphertext, after[i].ciphertext) ||
			!bytes.Equal(before[i].nonce, after[i].nonce) {
			t.Fatalf("failed rotation mutated row %s", before[i].id)
		}
	}
	// The original ring still decrypts the untouched data.
	got, err := st.Secrets.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("original plaintext lost: %v", err)
	}
	if got != "missing-version-secret" {
		t.Fatal("original plaintext mismatch")
	}
}

func TestRotateSecretsWrongCurrentKeyFailsClosed(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	seedSecret(t, st, "sandbox_password", "wrong-current-key-secret")
	// Version 1 exists but is the wrong key material: AAD/auth must fail.
	wrong := writeKeyringEnvelope(t, 1, map[int][]byte{1: bytes.Repeat([]byte{0x66}, 32)})
	before := readSecretRowsForTest(t, st.DBPath)
	_, err := RotateSecrets(ctx, RotationOptions{DBPath: st.DBPath, KeyRingPath: wrong})
	if err == nil {
		t.Fatal("rotation with a wrong current key must fail closed")
	}
	after := readSecretRowsForTest(t, st.DBPath)
	for i := range before {
		if !bytes.Equal(before[i].ciphertext, after[i].ciphertext) {
			t.Fatalf("failed rotation mutated row %s", before[i].id)
		}
	}
}

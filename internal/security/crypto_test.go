package security

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoxRoundTripAndAAD(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	ring, err := NewKeyRing(key)
	if err != nil {
		t.Fatal(err)
	}
	box := NewBox(ring)
	sealed, err := box.SealString("mailbox", "id-1", "super-secret-jwt")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.Ciphertext, []byte("super-secret-jwt")) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := box.OpenString("mailbox", "id-1", sealed)
	if err != nil || got != "super-secret-jwt" {
		t.Fatalf("open: %q %v", got, err)
	}
	// Different AAD must fail closed.
	if _, err := box.OpenString("mailbox", "id-2", sealed); err == nil {
		t.Fatal("expected AAD mismatch failure")
	}
	if _, err := box.OpenString("identity", "id-1", sealed); err == nil {
		t.Fatal("expected kind mismatch failure")
	}
	// Tampered ciphertext must fail.
	tampered := sealed
	tampered.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xff
	if _, err := box.OpenString("mailbox", "id-1", tampered); err == nil {
		t.Fatal("expected tamper failure")
	}
}

func TestBoxNonceUniqueness(t *testing.T) {
	ring, _ := NewKeyRing(bytes.Repeat([]byte{1}, 32))
	box := NewBox(ring)
	a, _ := box.SealString("k", "i", "v")
	b, _ := box.SealString("k", "i", "v")
	if bytes.Equal(a.Nonce, b.Nonce) {
		t.Fatal("nonce reused")
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("ciphertext reused with different nonce")
	}
}

func TestKeyRingRotation(t *testing.T) {
	oldKey := bytes.Repeat([]byte{2}, 32)
	newKey := bytes.Repeat([]byte{3}, 32)
	oldRing, _ := NewKeyRingWithVersions(oldKey, map[int][]byte{1: oldKey})
	sealed, err := NewBox(oldRing).SealString("k", "i", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	ring, err := NewKeyRingWithVersions(newKey, map[int][]byte{1: oldKey, 2: newKey})
	if err != nil {
		t.Fatal(err)
	}
	box := NewBox(ring)
	got, err := box.OpenString("k", "i", sealed)
	if err != nil || got != "legacy" {
		t.Fatalf("old version must stay decryptable: %v", err)
	}
	if ring.CurrentVersion() != 2 {
		t.Fatalf("current version = %d", ring.CurrentVersion())
	}
	// Unknown version fails closed.
	missing := sealed
	missing.KeyVersion = 9
	if _, err := box.OpenString("k", "i", missing); err == nil {
		t.Fatal("expected missing key version failure")
	}
}

func TestLoadKeyRingFormats(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{107}, 32)
	cases := map[string][]byte{
		"raw":    key,
		"hex":    []byte(strings.ToUpper(hexEncode(key))),
		"base64": []byte(b64Encode(key)),
		"json":   []byte(`{"current":1,"keys":{"1":"` + b64Encode(key) + `"}}`),
	}
	for name, content := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		ring, err := LoadKeyRing(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sealed, err := NewBox(ring).SealString("k", "i", "v")
		if err != nil {
			t.Fatalf("%s seal: %v", name, err)
		}
		if got, err := NewBox(ring).OpenString("k", "i", sealed); err != nil || got != "v" {
			t.Fatalf("%s open: %q %v", name, got, err)
		}
	}
}

func TestSessionSigner(t *testing.T) {
	ring, _ := NewKeyRing(bytes.Repeat([]byte{4}, 32))
	signer := NewSessionSigner(ring.keys[1], 3600)
	value, csrf, err := signer.Issue(1000)
	if err != nil {
		t.Fatal(err)
	}
	gotCSRF, ok := signer.Verify(value, 1001)
	if !ok || gotCSRF != csrf {
		t.Fatalf("verify failed: %v %q", ok, gotCSRF)
	}
	if _, ok := signer.Verify(value+"x", 1001); ok {
		t.Fatal("tampered session accepted")
	}
	if _, ok := signer.Verify(value, 1000+4000); ok {
		t.Fatal("expired session accepted")
	}
	if !ConstantTimeEqual("abc", "abc") || ConstantTimeEqual("abc", "abd") {
		t.Fatal("constant time equal broken")
	}
}

func TestRedactor(t *testing.T) {
	r := NewRedactor("top-secret-value")
	in := strings.Join([]string{
		"password=top-secret-value",
		"Authorization: Bearer abcdefghijklmnop",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnop",
		"state=xyz123&code=abc",
		"https://example.test/verify?token=SECRET123",
	}, " | ")
	out := r.Redact(in)
	for _, leak := range []string{"top-secret-value", "abcdefghijklmnop", "xyz123", "SECRET123", "eyJhbGciOiJIUzI1NiJ9"} {
		if strings.Contains(out, leak) {
			t.Fatalf("redaction leaked %q in %q", leak, out)
		}
	}
	if got := SanitizeURL("https://host/path?state=abc&token=def"); strings.Contains(got, "state") || strings.Contains(got, "token") {
		t.Fatalf("sanitize url leaked query: %s", got)
	}
	payload := r.SanitizeEventPayload(map[string]any{
		"provider": "fake", "access_token": "leak", "reason": "hello world", "body": "mail body",
	})
	if _, ok := payload["access_token"]; ok {
		t.Fatal("disallowed key persisted")
	}
	if _, ok := payload["body"]; ok {
		t.Fatal("message body key persisted")
	}
	if payload["provider"] != "fake" {
		t.Fatal("allowed key dropped")
	}
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0x0f]
	}
	return string(out)
}

func b64Encode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		out.WriteByte(alphabet[chunk[0]>>2])
		out.WriteByte(alphabet[((chunk[0]&0x03)<<4)|(chunk[1]>>4)])
		if n > 1 {
			out.WriteByte(alphabet[((chunk[1]&0x0f)<<2)|(chunk[2]>>6)])
		} else {
			out.WriteByte('=')
		}
		if n > 2 {
			out.WriteByte(alphabet[chunk[2]&0x3f])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

func TestVerifySessionReturnsCookieExpiryAndSessionID(t *testing.T) {
	ring, _ := NewKeyRing(bytes.Repeat([]byte{5}, 32))
	signer := NewSessionSigner(ring.keys[1], 10)
	value, csrf, sid, err := signer.IssueWithID(1000)
	if err != nil {
		t.Fatal(err)
	}
	gotCSRF, gotSID, exp, ok := signer.VerifySession(value, 1001)
	if !ok || gotCSRF != csrf || gotSID != sid || exp != 1010 {
		t.Fatalf("verify session = %q %q %d %v", gotCSRF, gotSID, exp, ok)
	}
	if _, _, _, ok := signer.VerifySession(value, 1020); ok {
		t.Fatal("expired session accepted")
	}
	// VerifyFull keeps the previous API contract.
	if gotCSRF, gotSID, ok := signer.VerifyFull(value, 1001); !ok || gotCSRF != csrf || gotSID != sid {
		t.Fatalf("VerifyFull = %q %q %v", gotCSRF, gotSID, ok)
	}
}

// Package security implements the cryptographic boundary: AES-256-GCM secret
// storage, signed browser sessions, CSRF tokens, constant-time comparisons and
// log redaction.
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// KeyRing holds versioned AES-256 keys. The current version is used for new
// ciphertexts; older versions remain decryptable until re-encryption completes.
type KeyRing struct {
	current int
	keys    map[int][]byte
}

// NewKeyRing builds a single-version ring.
func NewKeyRing(key []byte) (*KeyRing, error) {
	return NewKeyRingWithVersions(key, map[int][]byte{1: key})
}

// NewKeyRingWithVersions builds a ring from explicit versions. current must
// exist and every key must be 32 bytes.
func NewKeyRingWithVersions(current []byte, versions map[int][]byte) (*KeyRing, error) {
	if len(current) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	ring := &KeyRing{current: 1, keys: map[int][]byte{}}
	for v, k := range versions {
		if len(k) != 32 {
			return nil, fmt.Errorf("key version %d must be 32 bytes", v)
		}
		ring.keys[v] = append([]byte(nil), k...)
		if ring.current < v {
			ring.current = v
		}
	}
	if _, ok := ring.keys[ring.current]; !ok {
		return nil, errors.New("current key version is missing")
	}
	return ring, nil
}

// LoadKeyRing reads a key file. Supported formats:
//   - 32 raw bytes
//   - 64 hex characters
//   - base64 of 32 bytes
//   - JSON: {"current":2,"keys":{"1":"<base64>","2":"<base64>"}}
func LoadKeyRing(path string) (*KeyRing, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// A raw 32-byte key is accepted verbatim (it may contain whitespace bytes).
	if len(raw) == 32 {
		return NewKeyRing(raw)
	}
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "{") {
		var envelope struct {
			Current int               `json:"current"`
			Keys    map[string]string `json:"keys"`
		}
		if err := json.Unmarshal([]byte(s), &envelope); err != nil {
			return nil, fmt.Errorf("keyring json: %w", err)
		}
		if envelope.Current == 0 {
			envelope.Current = 1
		}
		versions := map[int][]byte{}
		for vs, encoded := range envelope.Keys {
			v, err := strconv.Atoi(vs)
			if err != nil {
				return nil, fmt.Errorf("keyring version %q: %w", vs, err)
			}
			key, err := decodeKey(encoded)
			if err != nil {
				return nil, fmt.Errorf("keyring version %s: %w", vs, err)
			}
			versions[v] = key
		}
		cur, ok := versions[envelope.Current]
		if !ok {
			return nil, fmt.Errorf("keyring current version %d missing", envelope.Current)
		}
		ring, err := NewKeyRingWithVersions(cur, versions)
		if err != nil {
			return nil, err
		}
		ring.current = envelope.Current
		return ring, nil
	}
	key, err := decodeKey(s)
	if err != nil {
		return nil, err
	}
	return NewKeyRing(key)
}

func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 32 {
		return []byte(s), nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, errors.New("key must be 32 bytes (raw/hex/base64)")
}

// CurrentVersion returns the version used for new ciphertexts.
func (k *KeyRing) CurrentVersion() int { return k.current }

// Box seals and opens secret values with AES-256-GCM.
type Box struct {
	ring *KeyRing
}

// NewBox returns a secret box.
func NewBox(ring *KeyRing) *Box { return &Box{ring: ring} }

// Sealed is an encrypted secret with its nonce and key version.
type Sealed struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int
}

// aad binds ciphertext to its logical location: kind + id + version.
func aad(kind, id string, version int) []byte {
	return []byte(fmt.Sprintf("account-onboarding|%s|%s|v%d", kind, id, version))
}

// Seal encrypts plaintext for (kind,id). A fresh 96-bit nonce is generated.
func (b *Box) Seal(kind, id string, plaintext []byte) (Sealed, error) {
	key, ok := b.ring.keys[b.ring.current]
	if !ok {
		return Sealed{}, errors.New("current key version unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Sealed{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Sealed{}, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad(kind, id, b.ring.current))
	return Sealed{Ciphertext: ct, Nonce: nonce, KeyVersion: b.ring.current}, nil
}

// Open decrypts a sealed secret. Unknown key versions fail closed.
func (b *Box) Open(kind, id string, sealed Sealed) ([]byte, error) {
	key, ok := b.ring.keys[sealed.KeyVersion]
	if !ok {
		return nil, fmt.Errorf("secret key version %d unavailable", sealed.KeyVersion)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed.Nonce) != gcm.NonceSize() {
		return nil, errors.New("invalid nonce size")
	}
	pt, err := gcm.Open(nil, sealed.Nonce, sealed.Ciphertext, aad(kind, id, sealed.KeyVersion))
	if err != nil {
		return nil, errors.New("secret decryption failed")
	}
	return pt, nil
}

// SealString is a convenience wrapper.
func (b *Box) SealString(kind, id, plaintext string) (Sealed, error) {
	return b.Seal(kind, id, []byte(plaintext))
}

// OpenString is a convenience wrapper.
func (b *Box) OpenString(kind, id string, sealed Sealed) (string, error) {
	pt, err := b.Open(kind, id, sealed)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// ---------------------------------------------------------------------------
// IDs and constant-time comparisons
// ---------------------------------------------------------------------------

// NewID returns a random RFC 4122 v4 UUID string.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RandomToken returns a URL-safe random token with n bytes of entropy.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ConstantTimeEqual compares two strings without early exit.
func ConstantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// HMACHex returns hex(HMAC-SHA256(key, data)); used for irreversible external
// resource identifiers and request fingerprints.
func HMACHex(key, data []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// DeriveKey derives a purpose-bound subkey from the master key.
func DeriveKey(master []byte, purpose string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte("account-onboarding|" + purpose))
	return m.Sum(nil)
}

// ---------------------------------------------------------------------------
// Session and CSRF
// ---------------------------------------------------------------------------

// SessionSigner issues and verifies HttpOnly session cookies carrying a CSRF
// token and expiry.
type SessionSigner struct {
	key []byte
	ttl int64 // seconds
}

type sessionPayload struct {
	Subject string `json:"sub"`
	Expiry  int64  `json:"exp"`
	CSRF    string `json:"csrf"`
	Nonce   string `json:"nonce"`
	// SessionID allows the server to revoke a session before its expiry
	// (for example on explicit logout).
	SessionID string `json:"sid"`
}

// NewSessionSigner derives a signing key from the master key.
func NewSessionSigner(master []byte, ttlSeconds int64) *SessionSigner {
	return &SessionSigner{key: DeriveKey(master, "session-v1"), ttl: ttlSeconds}
}

// Issue creates a signed cookie value and the matching CSRF token.
func (s *SessionSigner) Issue(nowUnix int64) (cookieValue, csrf string, err error) {
	value, csrf, _, err := s.IssueWithID(nowUnix)
	return value, csrf, err
}

// IssueWithID is Issue plus the session identifier used for revocation.
func (s *SessionSigner) IssueWithID(nowUnix int64) (cookieValue, csrf, sessionID string, err error) {
	csrf = RandomToken(32)
	sessionID = RandomToken(18)
	payload := sessionPayload{
		Subject:   "operator",
		Expiry:    nowUnix + s.ttl,
		CSRF:      csrf,
		Nonce:     RandomToken(16),
		SessionID: sessionID,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	sig := s.sign(encoded)
	return encoded + "." + sig, csrf, sessionID, nil
}

// Verify validates a cookie value and returns the embedded CSRF token.
func (s *SessionSigner) Verify(value string, nowUnix int64) (csrf string, ok bool) {
	csrf, _, ok = s.VerifyFull(value, nowUnix)
	return csrf, ok
}

// VerifyFull validates a cookie value and returns its CSRF token and session id.
func (s *SessionSigner) VerifyFull(value string, nowUnix int64) (csrf, sessionID string, ok bool) {
	csrf, sessionID, _, ok = s.VerifySession(value, nowUnix)
	return csrf, sessionID, ok
}

// VerifySession is VerifyFull plus the cookie's own expiry (unix seconds). The
// logout path needs the real cookie expiry so a later reduction of the session
// TTL cannot shorten a revocation below the lifetime of an already-issued
// cookie.
func (s *SessionSigner) VerifySession(value string, nowUnix int64) (csrf, sessionID string, expiresAt int64, ok bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return "", "", 0, false
	}
	expected := s.sign(parts[0])
	if !ConstantTimeEqual(expected, parts[1]) {
		return "", "", 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", 0, false
	}
	var payload sessionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", 0, false
	}
	if payload.Expiry < nowUnix || payload.CSRF == "" {
		return "", "", 0, false
	}
	return payload.CSRF, payload.SessionID, payload.Expiry, true
}

func (s *SessionSigner) sign(encoded string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

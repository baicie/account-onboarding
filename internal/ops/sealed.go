package ops

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/baicie/account-onboarding/internal/security"
)

// Encrypted snapshot file format (self-describing):
//
//	magic  8 bytes  "AONSEAL1"
//	len    4 bytes  big-endian uint32 length of the JSON header
//	header JSON     format/cipher/key_version/nonce/plaintext_sha256/plaintext_size
//	payload         AES-256-GCM ciphertext of the plaintext SQLite snapshot
//
// The AAD is bound by security.Box to kind="ops_snapshot" and
// id=<plaintext sha256>, so a tampered header (version or plaintext digest)
// fails authentication rather than silently decrypting a different file.
const (
	sealedMagic    = "AONSEAL1"
	sealedFormat   = "snapshot-encrypted-v1"
	sealedCipher   = "aes-256-gcm"
	sealedKind     = "ops_snapshot"
	sealedMaxHead  = 64 << 10
	sealedMaxPlain = int64(2) << 30 // 2 GiB safety bound for in-memory sealing
)

// sealedHeader is the JSON header inside an encrypted snapshot file.
type sealedHeader struct {
	Format          string `json:"format"`
	Cipher          string `json:"cipher"`
	KeyVersion      int    `json:"key_version"`
	Nonce           string `json:"nonce"`
	PlaintextSHA256 string `json:"plaintext_sha256"`
	PlaintextSize   int64  `json:"plaintext_size"`
}

func (h sealedHeader) validate() error {
	if h.Format != sealedFormat {
		return fmt.Errorf("unsupported sealed format %q", h.Format)
	}
	if h.Cipher != sealedCipher {
		return fmt.Errorf("unsupported sealed cipher %q", h.Cipher)
	}
	if h.KeyVersion < 1 {
		return fmt.Errorf("invalid sealed key version %d", h.KeyVersion)
	}
	if len(h.PlaintextSHA256) != 64 {
		return fmt.Errorf("invalid sealed plaintext sha256")
	}
	if h.PlaintextSize <= 0 || h.PlaintextSize > sealedMaxPlain {
		return fmt.Errorf("invalid sealed plaintext size %d", h.PlaintextSize)
	}
	if h.Nonce == "" {
		return errors.New("sealed header has no nonce")
	}
	return nil
}

// detectSealed reads only the file header. It returns (header, true, nil) for an
// encrypted snapshot, (zero, false, nil) for a plaintext one.
func detectSealed(path string) (sealedHeader, bool, error) {
	var h sealedHeader
	f, err := os.Open(path)
	if err != nil {
		return h, false, err
	}
	defer f.Close()
	prefix := make([]byte, len(sealedMagic)+4)
	if _, err := io.ReadFull(f, prefix); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return h, false, nil // too short to be sealed: plaintext/invalid
		}
		return h, false, err
	}
	if string(prefix[:len(sealedMagic)]) != sealedMagic {
		return h, false, nil
	}
	n := binary.BigEndian.Uint32(prefix[len(sealedMagic):])
	if n == 0 || n > sealedMaxHead {
		return h, true, fmt.Errorf("sealed snapshot header length %d is invalid", n)
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(f, raw); err != nil {
		return h, true, fmt.Errorf("read sealed header: %w", err)
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, true, fmt.Errorf("decode sealed header: %w", err)
	}
	if err := h.validate(); err != nil {
		return h, true, err
	}
	return h, true, nil
}

// sealSnapshot encrypts plaintextPath into sealedPath using the ring's current
// version. It returns the header, the ciphertext sha256 and its size.
func sealSnapshot(plaintextPath, sealedPath string, ring *security.KeyRing) (sealedHeader, string, int64, error) {
	var h sealedHeader
	if ring == nil {
		return h, "", 0, errors.New("seal snapshot: nil keyring")
	}
	st, err := os.Stat(plaintextPath)
	if err != nil {
		return h, "", 0, fmt.Errorf("seal snapshot: stat plaintext: %w", err)
	}
	if st.Size() <= 0 {
		return h, "", 0, errors.New("seal snapshot: plaintext is empty")
	}
	if st.Size() > sealedMaxPlain {
		return h, "", 0, fmt.Errorf("seal snapshot: plaintext is %d bytes (limit %d); keep backup encryption for smaller databases or size the tool accordingly", st.Size(), sealedMaxPlain)
	}
	plaintext, err := os.ReadFile(plaintextPath)
	if err != nil {
		return h, "", 0, fmt.Errorf("seal snapshot: read plaintext: %w", err)
	}
	defer zero(plaintext)
	sum := sha256.Sum256(plaintext)
	plainSHA := hex.EncodeToString(sum[:])

	box := security.NewBox(ring)
	sealed, err := box.Seal(sealedKind, plainSHA, plaintext)
	if err != nil {
		return h, "", 0, fmt.Errorf("seal snapshot: encrypt: %w", err)
	}
	header := sealedHeader{
		Format:          sealedFormat,
		Cipher:          sealedCipher,
		KeyVersion:      sealed.KeyVersion,
		Nonce:           base64.StdEncoding.EncodeToString(sealed.Nonce),
		PlaintextSHA256: plainSHA,
		PlaintextSize:   int64(len(plaintext)),
	}
	rawHeader, err := json.Marshal(header)
	if err != nil {
		return h, "", 0, fmt.Errorf("seal snapshot: encode header: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString(sealedMagic)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(rawHeader)))
	buf.Write(length[:])
	buf.Write(rawHeader)
	buf.Write(sealed.Ciphertext)

	if err := writeFileAtomic(sealedPath, buf.Bytes(), 0o600); err != nil {
		return h, "", 0, fmt.Errorf("seal snapshot: write sealed file: %w", err)
	}
	sealedSHA, sealedSize, err := hashFile(sealedPath)
	if err != nil {
		return h, "", 0, fmt.Errorf("seal snapshot: hash sealed file: %w", err)
	}
	return header, sealedSHA, sealedSize, nil
}

// decryptSealedFile reads and authenticates an encrypted snapshot entirely in
// memory. Callers must zero the returned plaintext.
func decryptSealedFile(sealedPath string, ring *security.KeyRing) ([]byte, sealedHeader, error) {
	var h sealedHeader
	if ring == nil {
		return nil, h, errors.New("encrypted snapshot requires a keyring")
	}
	detected, sealed, err := detectSealed(sealedPath)
	if err != nil {
		return nil, h, err
	}
	if !sealed {
		return nil, h, errors.New("snapshot is not encrypted")
	}
	h = detected
	nonce, err := base64.StdEncoding.DecodeString(h.Nonce)
	if err != nil {
		return nil, h, fmt.Errorf("decode sealed nonce: %w", err)
	}
	f, err := os.Open(sealedPath)
	if err != nil {
		return nil, h, err
	}
	defer f.Close()
	prefix := make([]byte, len(sealedMagic)+4)
	if _, err := io.ReadFull(f, prefix); err != nil {
		return nil, h, err
	}
	n := binary.BigEndian.Uint32(prefix[len(sealedMagic):])
	if _, err := f.Seek(int64(len(sealedMagic)+4)+int64(n), io.SeekStart); err != nil {
		return nil, h, err
	}
	ciphertext, err := io.ReadAll(io.LimitReader(f, sealedMaxPlain+1))
	if err != nil {
		return nil, h, fmt.Errorf("read sealed payload: %w", err)
	}
	if int64(len(ciphertext)) > sealedMaxPlain {
		return nil, h, errors.New("sealed payload exceeds the safety bound")
	}
	box := security.NewBox(ring)
	plaintext, err := box.Open(sealedKind, h.PlaintextSHA256, security.Sealed{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: h.KeyVersion,
	})
	if err != nil {
		return nil, h, fmt.Errorf("decrypt sealed snapshot: %w", err)
	}
	sum := sha256.Sum256(plaintext)
	if got := hex.EncodeToString(sum[:]); got != h.PlaintextSHA256 {
		zero(plaintext)
		return nil, h, fmt.Errorf("sealed plaintext sha256 mismatch: got %s want %s", got, h.PlaintextSHA256)
	}
	if int64(len(plaintext)) != h.PlaintextSize {
		zero(plaintext)
		return nil, h, fmt.Errorf("sealed plaintext size mismatch: got %d want %d", len(plaintext), h.PlaintextSize)
	}
	return plaintext, h, nil
}

// decryptSealedToPath decrypts an encrypted snapshot into dst (0600) and returns
// the plaintext sha256/size. dst is created exclusively and fsynced.
func decryptSealedToPath(sealedPath, dst string, ring *security.KeyRing) (string, int64, error) {
	plaintext, header, err := decryptSealedFile(sealedPath, ring)
	if err != nil {
		return "", 0, err
	}
	defer zero(plaintext)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create decrypted snapshot: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(dst)
	}
	if _, err := f.Write(plaintext); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("write decrypted snapshot: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", 0, fmt.Errorf("sync decrypted snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dst)
		return "", 0, err
	}
	return header.PlaintextSHA256, header.PlaintextSize, nil
}

// removeSensitiveFile best-effort overwrites a plaintext temp file before
// unlinking it. This is not a guarantee on SSD/COW filesystems; the encrypted
// snapshot remains the durable artifact.
func removeSensitiveFile(path string) {
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		if f, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			zeros := make([]byte, 64<<10)
			remaining := st.Size()
			for remaining > 0 {
				chunk := int64(len(zeros))
				if remaining < chunk {
					chunk = remaining
				}
				if _, err := f.Write(zeros[:chunk]); err != nil {
					break
				}
				remaining -= chunk
			}
			_ = f.Sync()
			_ = f.Close()
		}
	}
	_ = os.Remove(path)
}

// writeTempPlaintext writes plaintext bytes to a temp file in dir for read-only
// inspection (0600). The caller must call removeSensitiveFile.
func writeTempPlaintext(dir string, plaintext []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".onboard-plain-*.sqlite")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := f.Write(plaintext); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// sealedTempPath reserves a same-directory temp path (non-existent) for atomic
// publication.
func sealedTempPath(dir, base string) (string, error) {
	f, err := os.CreateTemp(dir, "."+filepath.Base(base)+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return name, nil
}

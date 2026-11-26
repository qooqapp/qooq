package watcher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"io"
	"strings"
	"testing"
)

// The production iteration count is deliberately slow; these tests check the
// scheme, and paying ~2s per derivation under -race buys no coverage.
func init() { pbkdf2Iterations = 4096 }

func TestEncryptDecryptRoundTrip(t *testing.T) {
	plaintext := "super-secret-token"
	passphrase := "my-passphrase"

	enc, err := encryptCredential(plaintext, passphrase)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if !strings.HasPrefix(enc, encPrefixV2) {
		t.Fatalf("new values should be written with %q, got %q", encPrefixV2, enc)
	}

	dec, err := decryptCredential(enc, passphrase)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}
	if dec != plaintext {
		t.Fatalf("round trip mismatch: got %q, want %q", dec, plaintext)
	}
}

// encryptV1 reproduces the original scheme (bare SHA-256 of the passphrase,
// no salt) so the compatibility path can be tested against a value this code
// can no longer produce. If this ever stops decrypting, every credential
// written before the upgrade is unreadable.
func encryptV1(t *testing.T, plaintext, passphrase string) string {
	t.Helper()
	key := deriveKeyV1(passphrase)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("v1 cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("v1 gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("v1 nonce: %v", err)
	}
	return encPrefixV1 + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil))
}

func TestDecryptV1WrittenBeforeTheUpgrade(t *testing.T) {
	plaintext := "token-from-an-older-watcher"
	old := encryptV1(t, plaintext, "my-passphrase")

	dec, err := decryptCredential(old, "my-passphrase")
	if err != nil {
		t.Fatalf("v1 value must still decrypt: %v", err)
	}
	if dec != plaintext {
		t.Fatalf("got %q, want %q", dec, plaintext)
	}

	if _, err := decryptCredential(old, "wrong-passphrase"); err == nil {
		t.Fatal("v1 value with the wrong passphrase should error")
	}
}

func TestEncryptCredentialSaltsEachValue(t *testing.T) {
	a, err := encryptCredential("same-plaintext", "same-passphrase")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	b, err := encryptCredential("same-plaintext", "same-passphrase")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if a == b {
		t.Fatal("two encryptions of the same value must differ: equal ciphertext leaks that two credentials are the same")
	}
	for _, enc := range []string{a, b} {
		dec, err := decryptCredential(enc, "same-passphrase")
		if err != nil || dec != "same-plaintext" {
			t.Fatalf("decrypt failed: %q, %v", dec, err)
		}
	}
}

func TestIsEncryptedRecognizesBothSchemes(t *testing.T) {
	v2, err := encryptCredential("x", "k")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	cases := map[string]bool{
		v2:                     true,
		encryptV1(t, "x", "k"): true,
		"plain-value":          false,
		"":                     false,
		"enc:v3:something":     false, // a scheme this build cannot read is not "encrypted" to it
	}
	for value, want := range cases {
		if got := isEncrypted(value); got != want {
			t.Fatalf("isEncrypted(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestDecryptCredentialBackwardCompatiblePlaintext(t *testing.T) {
	plaintext := "old-plaintext-value"
	dec, err := decryptCredential(plaintext, "some-passphrase")
	if err != nil {
		t.Fatalf("plaintext without prefix should pass through: %v", err)
	}
	if dec != plaintext {
		t.Fatalf("got %q, want %q", dec, plaintext)
	}

	// even with no passphrase configured at all
	dec, err = decryptCredential(plaintext, "")
	if err != nil || dec != plaintext {
		t.Fatalf("plaintext with empty passphrase should pass through unchanged: %q, %v", dec, err)
	}
}

func TestDecryptCredentialWrongKeyFails(t *testing.T) {
	enc, err := encryptCredential("secret-value", "correct-key")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if _, err := decryptCredential(enc, "wrong-key"); err == nil {
		t.Fatal("decrypt with wrong key should error, not panic or return garbage")
	}
}

func TestDecryptCredentialEncryptedButNoKey(t *testing.T) {
	enc, err := encryptCredential("secret-value", "some-key")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	if _, err := decryptCredential(enc, ""); err == nil {
		t.Fatal("decrypting an encrypted value with no configured passphrase should error")
	}
}

func TestDecryptCredentialTruncatedCiphertext(t *testing.T) {
	enc, err := encryptCredential("secret-value", "k")
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(enc, encPrefixV2))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Shorter than salt+nonce: must be rejected, not read out of bounds.
	short := encPrefixV2 + base64.StdEncoding.EncodeToString(raw[:saltLen+4])
	if _, err := decryptCredential(short, "k"); err == nil {
		t.Fatal("truncated ciphertext should error")
	}
}

func TestEncryptCredentialEmptyInputsPassThrough(t *testing.T) {
	if got, err := encryptCredential("", "key"); err != nil || got != "" {
		t.Fatalf("empty plaintext should pass through: %q, %v", got, err)
	}
	if got, err := encryptCredential("plaintext", ""); err != nil || got != "plaintext" {
		t.Fatalf("empty passphrase should leave plaintext unchanged: %q, %v", got, err)
	}
}

package watcher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Stored credentials carry the scheme that wrote them, so the key derivation
// can be strengthened without a schema migration and without invalidating
// anything already in the database.
//
//   - enc:v1: base64(nonce+ciphertext), key = SHA-256(passphrase).
//     Still read, never written. A single SHA-256 is one guess per hash for
//     anyone who obtains watcher.db, which is exactly the offline attack a
//     passphrase needs to be slow against.
//   - enc:v2: base64(salt+nonce+ciphertext), key = PBKDF2-HMAC-SHA256.
//     The salt is per-value and stored with the ciphertext, so the blob stays
//     self-describing: nothing outside it has to be kept in sync for it to be
//     readable.
const (
	encPrefixV1 = "enc:v1:"
	encPrefixV2 = "enc:v2:"

	saltLen = 16
	keyLen  = 32 // AES-256
)

// pbkdf2Iterations follows OWASP's guidance for PBKDF2-HMAC-SHA256. Deriving
// costs a fraction of a second, which is why derivedKeys exists: the sweep
// loop decrypts the same credentials every 10s and must not pay it each time.
//
// A var, not a const, only so tests can lower it — they exercise the scheme,
// not its cost, and the real count adds seconds per derivation under -race for
// no extra coverage. Nothing outside a test may write it.
var pbkdf2Iterations = 600_000

// errCredentialDecrypt is wrapped into every decryption failure so callers
// can errors.Is() it: it always means "the secret key changed (or was
// unset) since this credential was encrypted" — never a corrupt/missing
// value, which would be a plain validation error instead.
var errCredentialDecrypt = errors.New("watcher: credential decryption failed (secret key changed or missing?)")

// derivedKeys memoizes PBKDF2 output, which is deliberately expensive and
// deterministic. Keyed by passphrase+salt; the map is capped and cleared
// wholesale when it fills, because entries only accumulate as credentials are
// rewritten (each write mints a fresh salt) and the ones still in the database
// are re-derived on their next use.
var derivedKeys struct {
	sync.Mutex
	m map[string][]byte
}

const derivedKeysMax = 128

// isEncrypted reports whether a stored value carries a ciphertext prefix.
// Anything else is plaintext: written before a secret key was configured, or
// while none is. Use this rather than testing one prefix — a check that knows
// only about v1 silently treats v2 ciphertext as a plaintext value, which is
// how ciphertext ends up displayed as if it were a password.
func isEncrypted(value string) bool {
	return strings.HasPrefix(value, encPrefixV1) || strings.HasPrefix(value, encPrefixV2)
}

// deriveKeyV1 is the original scheme: a bare SHA-256 of the passphrase.
// Retained to read existing rows; never used to write.
func deriveKeyV1(passphrase string) []byte {
	sum := sha256.Sum256([]byte(passphrase))
	return sum[:]
}

// deriveKeyV2 stretches the passphrase with PBKDF2 so that guessing it against
// a stolen database costs the attacker the same work per guess that it costs
// the watcher once.
func deriveKeyV2(passphrase string, salt []byte) ([]byte, error) {
	cacheKey := passphrase + "\x00" + string(salt)

	derivedKeys.Lock()
	if key, ok := derivedKeys.m[cacheKey]; ok {
		derivedKeys.Unlock()
		return key, nil
	}
	derivedKeys.Unlock()

	key, err := pbkdf2.Key(sha256.New, passphrase, salt, pbkdf2Iterations, keyLen)
	if err != nil {
		return nil, err
	}

	derivedKeys.Lock()
	if derivedKeys.m == nil || len(derivedKeys.m) >= derivedKeysMax {
		derivedKeys.m = make(map[string][]byte, derivedKeysMax)
	}
	derivedKeys.m[cacheKey] = key
	derivedKeys.Unlock()
	return key, nil
}

// encryptCredential AES-256-GCM-encrypts plaintext under passphrase and
// returns the enc:v2:-prefixed, base64-encoded result. An empty plaintext or
// passphrase is returned unchanged (nothing to protect / no key configured).
func encryptCredential(plaintext, passphrase string) (string, error) {
	if plaintext == "" || passphrase == "" {
		return plaintext, nil
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("encrypt credential: generate salt: %w", err)
	}
	key, err := deriveKeyV2(passphrase, salt)
	if err != nil {
		return "", fmt.Errorf("encrypt credential: derive key: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", fmt.Errorf("encrypt credential: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encrypt credential: generate nonce: %w", err)
	}
	// Seal appends to salt+nonce so the blob carries everything decryption
	// needs: no external state, no ordering assumption between rows.
	blob := gcm.Seal(append(salt, nonce...), nonce, []byte(plaintext), nil)
	return encPrefixV2 + base64.StdEncoding.EncodeToString(blob), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// decryptCredential reverses encryptCredential. A value without the enc:v1:
// prefix is assumed to be plaintext (either QOOQ_WATCHER_SECRET_KEY was
// never set, or this row predates it) and is returned as-is — this is the
// backward-compatibility path: old plaintext rows keep working unmodified
// until upgradePlaintextCredentials (or the next write) re-encrypts them.
// An encrypted value with no configured passphrase, or one that fails to
// authenticate (wrong key), returns errCredentialDecrypt rather than
// panicking or silently returning garbage.
func decryptCredential(value, passphrase string) (string, error) {
	if value == "" || !isEncrypted(value) {
		return value, nil
	}
	if passphrase == "" {
		return "", fmt.Errorf("%w: value is encrypted but no secret.key is configured", errCredentialDecrypt)
	}

	v2 := strings.HasPrefix(value, encPrefixV2)
	prefix := encPrefixV1
	if v2 {
		prefix = encPrefixV2
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("%w: %v", errCredentialDecrypt, err)
	}

	var key []byte
	if v2 {
		if len(raw) < saltLen {
			return "", fmt.Errorf("%w: ciphertext too short", errCredentialDecrypt)
		}
		var salt []byte
		salt, raw = raw[:saltLen], raw[saltLen:]
		if key, err = deriveKeyV2(passphrase, salt); err != nil {
			return "", fmt.Errorf("%w: %v", errCredentialDecrypt, err)
		}
	} else {
		key = deriveKeyV1(passphrase)
	}

	gcm, err := newGCM(key)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errCredentialDecrypt, err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("%w: ciphertext too short", errCredentialDecrypt)
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// gcm.Open's own error never includes the plaintext/key, safe to wrap.
		return "", fmt.Errorf("%w: %v", errCredentialDecrypt, err)
	}
	return string(plain), nil
}

// decryptTarget decrypts t.AuthCredential and t.WebhookSecret in place using
// w.secretKey, so every other package (git.go, webhook.go) only ever sees
// plaintext. Called right after a Target is loaded from the DB and before
// its credentials are used for anything (sweep.go's sweepOnce, webhook.go's
// handleWebhook). Plaintext rows (no enc:v1: prefix) pass through unchanged
// regardless of whether secretKey is set.
func (w *Watcher) decryptTarget(t *Target) error {
	cred, err := decryptCredential(t.AuthCredential, w.secretKey)
	if err != nil {
		return fmt.Errorf("auth_credential: %w", err)
	}
	t.AuthCredential = cred

	secret, err := decryptCredential(t.WebhookSecret, w.secretKey)
	if err != nil {
		return fmt.Errorf("webhook_secret: %w", err)
	}
	t.WebhookSecret = secret
	return nil
}

// upgradePlaintextCredentials runs once at startup when secretKey is
// non-empty. It brings every target's AuthCredential/WebhookSecret up to the
// current scheme: plaintext rows (written before QOOQ_WATCHER_SECRET_KEY was
// set) get encrypted, and enc:v1: rows get re-encrypted under the stronger
// key derivation. New/updated targets are already current at write time, so
// this has work to do only after the key is first introduced, or once after
// an upgrade that changes the scheme.
//
// Values that will not decrypt are left exactly as they are: that means the
// passphrase changed, and overwriting them would destroy the only copy.
// Other tables (stacks, service state, input values) are not swept — v1 stays
// readable indefinitely, and those rotate to v2 the next time they are
// written. What is swept here is what an attacker with the file most wants.
func (w *Watcher) upgradePlaintextCredentials() error {
	var targets []Target
	if err := w.db.Find(&targets).Error; err != nil {
		return fmt.Errorf("list targets: %w", err)
	}

	for _, t := range targets {
		updates := map[string]any{}
		for _, f := range []struct {
			column string
			value  string
		}{
			{"auth_credential", t.AuthCredential},
			{"webhook_secret", t.WebhookSecret},
		} {
			if f.value == "" || strings.HasPrefix(f.value, encPrefixV2) {
				continue
			}
			plain, err := decryptCredential(f.value, w.secretKey)
			if err != nil {
				w.log.Warn("upgrade credentials: cannot decrypt, leaving as-is",
					zap.String("target", t.Name), zap.String("field", f.column), zap.Error(err))
				continue
			}
			enc, err := encryptCredential(plain, w.secretKey)
			if err != nil {
				w.log.Warn("upgrade credentials: encrypt failed",
					zap.String("target", t.Name), zap.String("field", f.column), zap.Error(err))
				continue
			}
			updates[f.column] = enc
		}

		if len(updates) == 0 {
			continue
		}
		if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(updates).Error; err != nil {
			w.log.Warn("upgrade credentials: persist failed", zap.String("target", t.Name), zap.Error(err))
			continue
		}
		w.log.Info("upgraded target credentials to the current encryption scheme", zap.String("target", t.Name))
	}
	return nil
}

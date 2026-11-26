package watcher

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// errWebhookNotConfigured, errWebhookSignatureMissing and
// errWebhookBadSignature are the three ways signature verification can fail;
// handleWebhook maps them to distinct HTTP statuses.
var (
	errWebhookNotConfigured    = errors.New("webhook not configured")
	errWebhookSignatureMissing = errors.New("missing webhook signature")
	errWebhookBadSignature     = errors.New("invalid webhook signature")
)

// handleWebhook implements the "poke" model: verify the request's signature
// against the target's webhook_secret, then just trigger an immediate check
// (triggerNow) — the payload itself is never parsed or trusted. This mirrors
// GitHub/GitLab's own recommendation of treating webhooks as a nudge to go
// look at the source of truth, not as the source of truth.
func (w *Watcher) handleWebhook(c *gin.Context) {
	name := c.Param("name")

	t, err := getTargetByName(w.db, name)
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}

	// M18: WebhookSecret may be AES-GCM-encrypted at rest; decrypt before use.
	// A decrypt failure (secret.key changed since this target was created) is
	// a distinct, clear error rather than treating garbage ciphertext as "no
	// secret configured" (which would silently 403 instead of explaining why).
	if secret, err := decryptCredential(t.WebhookSecret, w.secretKey); err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("webhook_secret: %w", err))
		return
	} else {
		t.WebhookSecret = secret
	}

	const maxWebhookBodyBytes = 1 << 20 // 1MB: webhook payloads are small JSON pokes, never trusted for content anyway
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			respondErr(c, http.StatusRequestEntityTooLarge, err)
			return
		}
		respondErr(c, http.StatusBadRequest, err)
		return
	}

	if err := verifyWebhookSignature(c.Request.Header, body, t.WebhookSecret); err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, errWebhookNotConfigured) {
			status = http.StatusForbidden
		}
		respondErr(c, status, err)
		return
	}

	if err := triggerNow(w.db, name); err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Status(http.StatusAccepted)
}

// verifyWebhookSignature picks the verification scheme by which header is
// present (GitHub's X-Hub-Signature-256 or GitLab's X-Gitlab-Token) and
// checks it against secret using constant-time comparison. A target with no
// webhook_secret configured always rejects (403 upstream), regardless of
// whether a header was sent — there is nothing to check the signature
// against. A configured secret with neither header present is a 401 (caller
// didn't even try to authenticate).
func verifyWebhookSignature(h http.Header, body []byte, secret string) error {
	if secret == "" {
		return errWebhookNotConfigured
	}

	githubSig := h.Get("X-Hub-Signature-256")
	gitlabToken := h.Get("X-Gitlab-Token")

	switch {
	case githubSig != "":
		const prefix = "sha256="
		if !strings.HasPrefix(githubSig, prefix) {
			return errWebhookBadSignature
		}
		got, err := hex.DecodeString(strings.TrimPrefix(githubSig, prefix))
		if err != nil {
			return errWebhookBadSignature
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := mac.Sum(nil)
		if !hmac.Equal(got, want) {
			return errWebhookBadSignature
		}
		return nil
	case gitlabToken != "":
		if !hmac.Equal([]byte(gitlabToken), []byte(secret)) {
			return errWebhookBadSignature
		}
		return nil
	default:
		return errWebhookSignatureMissing
	}
}

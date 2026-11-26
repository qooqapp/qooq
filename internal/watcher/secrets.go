package watcher

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// The per-namespace secret store, the second half of what
// makes user isolation real.
//
// The hole it closes: ${secret.NAME} used to read the watcher process's
// QOOQ_SECRET_<NAME>, and ${env.NAME} still reads its whole environment. On a
// watcher serving one operator that is a convenience. On a watcher serving more
// than one user it means a manifest in user A's repository can read
// user B's credentials, which is the most severe of the three shared-node
// blockers.
//
// The syntax was frozen ahead of this deliberately (see interpolate.go): manifests
// live in other people's repositories and cannot be migrated, so ${secret.NAME}
// had to mean the right thing before the store behind it was real. Nothing any
// user has written needs to change now that it is.

// Secret is one named value belonging to one namespace.
//
// Value is encrypted with the same M18 machinery as a target's credentials, so
// there is one answer to "how are secrets at rest protected?" rather than two
// that can drift apart.
type Secret struct {
	// An autoincrement uint, matching Target and Stack in this package rather
	// than the ULIDs the daemon and external manager use. A secret is addressed by
	// (namespace, name) everywhere that matters; the id is only gorm's handle.
	ID uint `gorm:"primaryKey" json:"-"`

	// Namespace is the qualified namespace on a shared node ("acme--prod") and the
	// plain one offline ("default"). The watcher does not care which: it is an
	// opaque scope, exactly as everywhere else.
	Namespace string `gorm:"not null;uniqueIndex:idx_secret_ns_name" json:"namespace"`
	Name      string `gorm:"not null;uniqueIndex:idx_secret_ns_name" json:"name"`

	// Value never appears in JSON. This struct is returned by the list API, and
	// a listing that includes values is a credential dump with pagination.
	Value string `gorm:"not null" json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var errSecretNotFound = errors.New("secret not found")

// putSecret stores or replaces a namespace's secret.
func (w *Watcher) putSecret(namespace, name, plaintext string) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("namespace and name are required")
	}
	enc, err := encryptCredential(plaintext, w.secretKey)
	if err != nil {
		return fmt.Errorf("encrypt secret: %w", err)
	}

	var existing Secret
	err = w.db.Where("namespace = ? AND name = ?", namespace, name).First(&existing).Error
	switch {
	case err == nil:
		return w.db.Model(&Secret{}).Where("id = ?", existing.ID).
			Update("value", enc).Error
	case errors.Is(err, gorm.ErrRecordNotFound):
		return w.db.Create(&Secret{
			Namespace: namespace,
			Name:      name,
			Value:     enc,
		}).Error
	default:
		return fmt.Errorf("look up secret: %w", err)
	}
}

// getSecret returns a namespace's secret in the clear.
func (w *Watcher) getSecret(namespace, name string) (string, error) {
	var s Secret
	err := w.db.Where("namespace = ? AND name = ?", namespace, name).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", errSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("look up secret: %w", err)
	}
	// A value that will not decrypt means the passphrase changed. Reported, not
	// silently treated as absent: falling through to the process environment
	// here would resolve a user's reference with somebody else's value.
	return decryptCredential(s.Value, w.secretKey)
}

// listSecretNames returns the names a namespace has, never the values.
func (w *Watcher) listSecretNames(namespace string) ([]string, error) {
	var out []string
	if err := w.db.Model(&Secret{}).
		Where("namespace = ?", namespace).
		Order("name").
		Pluck("name", &out).Error; err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	return out, nil
}

func (w *Watcher) deleteSecret(namespace, name string) error {
	res := w.db.Where("namespace = ? AND name = ?", namespace, name).Delete(&Secret{})
	if res.Error != nil {
		return fmt.Errorf("delete secret: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return errSecretNotFound
	}
	return nil
}

// distinctTargetNamespaces counts how many namespaces this watcher has targets
// in.
//
// Used only to decide whether the process-environment warning is worth
// printing: on a single-namespace install, reading the watcher's environment is
// the intended behaviour and warning about it every start would be the kind of
// routinely-harmless warning this project has twice decided not to emit. More
// than one namespace is the first real evidence that the environment is shared.
//
// Counted in Go rather than SQL because the namespace lives inside the
// deploy_spec JSON, and a DISTINCT over that column would count specs, not
// namespaces.
func (w *Watcher) distinctTargetNamespaces() (int, error) {
	var targets []Target
	if err := w.db.Find(&targets).Error; err != nil {
		return 0, err
	}
	seen := make(map[string]struct{}, len(targets))
	for i := range targets {
		seen[namespaceOf(&targets[i])] = struct{}{}
	}
	return len(seen), nil
}

// secretResolver resolves ${secret.NAME} for one deploy.
//
// Bound to a single namespace at construction. That is the isolation: there is
// no argument by which a manifest can ask for a different one, because the
// resolver it is handed cannot express the question.
type secretResolver struct {
	w         *Watcher
	namespace string
	// allowProcessEnv keeps the pre-M3 behaviour available for the single
	// operator case, where the watcher's environment belongs to the one person
	// deploying and reading it is a feature rather than a leak.
	allowProcessEnv bool
}

func (w *Watcher) newSecretResolver(namespace string) *secretResolver {
	return &secretResolver{
		w:               w,
		namespace:       namespace,
		allowProcessEnv: w.allowProcessEnv,
	}
}

// lookup resolves one ${secret.NAME}.
//
// Store first, process environment second. The order matters: a namespace that
// has defined a secret must get its own value even when the watcher happens to
// have an environment variable of the same name — otherwise one operator's
// stray QOOQ_SECRET_DB_PASSWORD silently overrides every user's.
func (r *secretResolver) lookup(name string) (string, bool) {
	if name == "" {
		return "", false
	}

	// A resolver with no store is process-environment only. That is a real
	// state, not a test contrivance: it is what a scope built for interpolation
	// syntax alone has, and what an install with no secrets set behaves like.
	if r.w == nil {
		if !r.allowProcessEnv {
			return "", false
		}
		return os.LookupEnv(secretEnvPrefix + name)
	}

	v, err := r.w.getSecret(r.namespace, name)
	switch {
	case err == nil:
		return v, true
	case errors.Is(err, errSecretNotFound):
		// fall through to the environment, if allowed
	default:
		// A decryption failure is not "absent". Refusing to resolve makes the
		// deploy fail with an unknown-reference error, which is a bad message
		// for this cause — so it is logged explicitly here.
		r.w.log.Error("a stored secret could not be decrypted (has the secret key changed?)",
			zap.String("namespace", r.namespace), zap.String("name", name), zap.Error(err))
		return "", false
	}

	if !r.allowProcessEnv {
		return "", false
	}
	return os.LookupEnv(secretEnvPrefix + name)
}

// lookupEnv resolves one ${env.NAME}.
//
// Gated by the same switch as the secret fallback, because they are the same
// hazard wearing two names: both read the watcher process's environment, which
// on a shared watcher belongs to the deployment and not to any one user.
func (r *secretResolver) lookupEnv(name string) (string, bool) {
	if !r.allowProcessEnv {
		return "", false
	}
	return os.LookupEnv(name)
}

// explainUnresolved says why a secret or env reference did not resolve.
//
// Without it, a manifest that correctly names a secret nobody has set yet fails
// with "unknown reference", which reads as a typo and sends the author looking
// at their own spelling. The distinction — the name is fine, the value is
// missing — is the whole difference between a two-minute fix and an afternoon.
func (r *secretResolver) explainUnresolved(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, "secret.")
	if ok {
		if !r.allowProcessEnv {
			return fmt.Sprintf("no secret named %q in namespace %q "+
				"(set it with PUT /v1/namespaces/%s/secrets/%s)",
				name, r.namespace, r.namespace, name), true
		}
		return fmt.Sprintf("no secret named %q in namespace %q, and no %s%s in the watcher's environment",
			name, r.namespace, secretEnvPrefix, name), true
	}
	if name, ok := strings.CutPrefix(ref, "env."); ok {
		if !r.allowProcessEnv {
			return fmt.Sprintf("${env.%s} is disabled on this watcher: it reads the watcher's own "+
				"environment, which on a shared watcher is not this namespace's to read. "+
				"Use ${secret.%s} instead", name, name), true
		}
		return fmt.Sprintf("no %s in the watcher's environment", name), true
	}
	return "", false
}

// processEnvOnlyResolver is a resolver with no store behind it: ${secret.NAME}
// falls straight through to QOOQ_SECRET_<NAME> and ${env.NAME} to the process
// environment.
//
// Used by tests of the interpolation syntax, which have no database and are not
// about the store. Production paths always build one with newSecretResolver, so
// they always have the store.
func processEnvOnlyResolver() *secretResolver {
	return &secretResolver{allowProcessEnv: true}
}

package watcher

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/glebarez/sqlite"
)

// secretWatcher is a Watcher with a real sqlite behind it, because the store is
// the thing under test.
func secretWatcher(t *testing.T, allowProcessEnv bool) *Watcher {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/w.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Target{}, &Stack{}, &Secret{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &Watcher{
		log:             zap.NewNop(),
		db:              db,
		secretKey:       "test-passphrase",
		allowProcessEnv: allowProcessEnv,
	}
}

// The hole M3 closes, stated as a test: one namespace's manifest must not be
// able to read another namespace's secret. Before the per-namespace store,
// ${secret.NAME} read the watcher's own environment, which every user'shared.
func TestOneNamespaceCannotReadAnothersSecret(t *testing.T) {
	w := secretWatcher(t, false)

	if err := w.putSecret("alice--prod", "DB_PASSWORD", "alices-password"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := w.putSecret("bob--prod", "DB_PASSWORD", "bobs-password"); err != nil {
		t.Fatalf("put: %v", err)
	}

	alice := w.newSecretResolver("alice--prod")
	bob := w.newSecretResolver("bob--prod")

	if got, ok := alice.lookup("DB_PASSWORD"); !ok || got != "alices-password" {
		t.Fatalf("alice got %q (%v), want her own", got, ok)
	}
	if got, ok := bob.lookup("DB_PASSWORD"); !ok || got != "bobs-password" {
		t.Fatalf("bob got %q (%v), want his own", got, ok)
	}

	// A namespace with no such secret does not fall through to anyone else's.
	carol := w.newSecretResolver("carol--prod")
	if got, ok := carol.lookup("DB_PASSWORD"); ok {
		t.Fatalf("a namespace with no secret of that name resolved to %q", got)
	}
}

// The resolver is bound to one namespace at construction, so a manifest has no
// argument by which to ask about a different one.
func TestAResolverCannotBeAskedAboutAnotherNamespace(t *testing.T) {
	w := secretWatcher(t, false)
	if err := w.putSecret("bob--prod", "TOKEN", "bobs"); err != nil {
		t.Fatalf("put: %v", err)
	}

	alice := w.newSecretResolver("alice--prod")
	// Every spelling that might smuggle a namespace through the name.
	for _, name := range []string{
		"TOKEN",
		"bob--prod/TOKEN",
		"bob--prod.TOKEN",
		"../bob--prod/TOKEN",
	} {
		if got, ok := alice.lookup(name); ok {
			t.Errorf("alice.lookup(%q) resolved to %q", name, got)
		}
	}
}

// ${env.*} is the other half of the same hazard and must be gateable.
func TestProcessEnvIsGatedByTheSameSwitch(t *testing.T) {
	t.Setenv("QOOQ_SECRET_SHARED", "from-environment")
	t.Setenv("PLAIN_VALUE", "from-environment")

	allowed := secretWatcher(t, true).newSecretResolver("alice--prod")
	if got, ok := allowed.lookup("SHARED"); !ok || got != "from-environment" {
		t.Errorf("with the switch on, the QOOQ_SECRET_ fallback should work: %q %v", got, ok)
	}
	if got, ok := allowed.lookupEnv("PLAIN_VALUE"); !ok || got != "from-environment" {
		t.Errorf("with the switch on, ${env.*} should work: %q %v", got, ok)
	}

	denied := secretWatcher(t, false).newSecretResolver("alice--prod")
	if got, ok := denied.lookup("SHARED"); ok {
		t.Errorf("with the switch off, the QOOQ_SECRET_ fallback still resolved: %q", got)
	}
	if got, ok := denied.lookupEnv("PLAIN_VALUE"); ok {
		t.Errorf("with the switch off, ${env.*} still read the environment: %q", got)
	}
}

// A namespace's own secret must win over a same-named process variable.
// Otherwise one stray QOOQ_SECRET_DB_PASSWORD on the host silently overrides
// what every user'set for themselves.
func TestTheStoreWinsOverTheProcessEnvironment(t *testing.T) {
	t.Setenv("QOOQ_SECRET_DB_PASSWORD", "the-hosts-value")

	w := secretWatcher(t, true)
	if err := w.putSecret("alice--prod", "DB_PASSWORD", "alices-value"); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok := w.newSecretResolver("alice--prod").lookup("DB_PASSWORD")
	if !ok || got != "alices-value" {
		t.Fatalf("resolved to %q, want the namespace's own value", got)
	}
}

// Values are encrypted at rest with the same machinery as target credentials,
// so there is one answer to "how are secrets protected?" rather than two.
func TestSecretsAreEncryptedAtRest(t *testing.T) {
	w := secretWatcher(t, false)
	if err := w.putSecret("alice--prod", "TOKEN", "super-secret-value"); err != nil {
		t.Fatalf("put: %v", err)
	}

	var raw Secret
	if err := w.db.First(&raw, "namespace = ? AND name = ?", "alice--prod", "TOKEN").Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(raw.Value, "super-secret-value") {
		t.Fatalf("the plaintext is in the database: %q", raw.Value)
	}
	if !strings.HasPrefix(raw.Value, "enc:") {
		t.Fatalf("stored value is not encrypted: %q", raw.Value)
	}
}

// A listing must never carry values.
func TestListingSecretsReturnsNamesOnly(t *testing.T) {
	w := secretWatcher(t, false)
	for _, n := range []string{"B_TOKEN", "A_TOKEN"} {
		if err := w.putSecret("alice--prod", n, "value-of-"+n); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if err := w.putSecret("bob--prod", "C_TOKEN", "bobs"); err != nil {
		t.Fatalf("put: %v", err)
	}

	names, err := w.listSecretNames("alice--prod")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 2 || names[0] != "A_TOKEN" || names[1] != "B_TOKEN" {
		t.Fatalf("names = %v, want alice's two, sorted", names)
	}
}

func TestPutSecretReplacesRatherThanDuplicating(t *testing.T) {
	w := secretWatcher(t, false)
	if err := w.putSecret("alice--prod", "TOKEN", "first"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := w.putSecret("alice--prod", "TOKEN", "second"); err != nil {
		t.Fatalf("put again: %v", err)
	}

	got, err := w.getSecret("alice--prod", "TOKEN")
	if err != nil || got != "second" {
		t.Fatalf("got %q, %v; want the replacement", got, err)
	}
	var n int64
	w.db.Model(&Secret{}).Where("namespace = ? AND name = ?", "alice--prod", "TOKEN").Count(&n)
	if n != 1 {
		t.Fatalf("%d rows for one (namespace, name)", n)
	}
}

// Two namespaces may each have a secret of the same name — that is the whole
// point — so the unique index must be on the pair.
func TestTheSameNameInTwoNamespacesIsNotAConflict(t *testing.T) {
	w := secretWatcher(t, false)
	if err := w.putSecret("alice--prod", "TOKEN", "a"); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if err := w.putSecret("bob--prod", "TOKEN", "b"); err != nil {
		t.Fatalf("bob must be allowed the same name: %v", err)
	}
}

// A value that will not decrypt must not fall through to the process
// environment: resolving a user's reference with somebody else's value is
// worse than failing.
func TestAnUndecryptableSecretDoesNotFallThroughToTheEnvironment(t *testing.T) {
	t.Setenv("QOOQ_SECRET_TOKEN", "the-hosts-value")

	w := secretWatcher(t, true)
	if err := w.putSecret("alice--prod", "TOKEN", "alices"); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Simulate the passphrase having changed since the value was written.
	w.secretKey = "a-different-passphrase"

	if got, ok := w.newSecretResolver("alice--prod").lookup("TOKEN"); ok {
		t.Fatalf("an undecryptable secret resolved to %q", got)
	}
}

// The message when a reference does not resolve has to say which of the two
// reasons it is: the name is wrong, or the value is not set. "unknown
// reference" alone sends the author to check their own spelling.
func TestUnresolvedReferencesExplainThemselves(t *testing.T) {
	denied := secretWatcher(t, false).newSecretResolver("alice--prod")

	msg, ok := denied.explainUnresolved("secret.STRIPE")
	if !ok || !strings.Contains(msg, "alice--prod") {
		t.Errorf("secret explanation does not name the namespace: %q", msg)
	}
	msg, ok = denied.explainUnresolved("env.ANYTHING")
	if !ok || !strings.Contains(msg, "disabled") {
		t.Errorf("env explanation does not say it is disabled: %q", msg)
	}
	if _, ok := denied.explainUnresolved("service.db.host"); ok {
		t.Error("it claimed to explain a reference it does not own")
	}
}

func TestDeleteSecretIsScopedToItsNamespace(t *testing.T) {
	w := secretWatcher(t, false)
	if err := w.putSecret("alice--prod", "TOKEN", "a"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := w.deleteSecret("bob--prod", "TOKEN"); err == nil {
		t.Error("deleting from another namespace reported success")
	}
	if _, err := w.getSecret("alice--prod", "TOKEN"); err != nil {
		t.Fatalf("alice's secret was removed by bob's delete: %v", err)
	}
}

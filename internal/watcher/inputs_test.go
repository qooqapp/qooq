package watcher

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseManifestInputs(t *testing.T) {
	mf, err := parseManifest([]byte(`
version: 1
app: demo
inputs:
  PUBLIC_HOST:
    description: where a browser reaches this
    default: ${host}
    example: 192.168.1.50
  STRIPE_KEY:
    description: Stripe secret key
    required: true
    secret: true
`))
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	if len(mf.Inputs) != 2 {
		t.Fatalf("got %d inputs, want 2", len(mf.Inputs))
	}
	if got := mf.Inputs["PUBLIC_HOST"].Default; got != "${host}" {
		t.Errorf("PUBLIC_HOST default = %q, want ${host}", got)
	}
	if !mf.Inputs["STRIPE_KEY"].Required || !mf.Inputs["STRIPE_KEY"].Secret {
		t.Errorf("STRIPE_KEY = %+v, want required and secret", mf.Inputs["STRIPE_KEY"])
	}
}

func TestMissingInputs(t *testing.T) {
	declared := map[string]manifestInput{
		"REQUIRED_UNSET": {Required: true},
		"REQUIRED_SET":   {Required: true},
		"OPTIONAL":       {},
		"HAS_DEFAULT":    {Default: "127.0.0.1"},
		"REQUIRED_BLANK": {Required: true},
	}
	values := inputValues{
		"REQUIRED_SET":   "value",
		"REQUIRED_BLANK": "", // present but empty is still missing
	}

	got := missingInputs(declared, values)
	want := []string{"REQUIRED_BLANK", "REQUIRED_UNSET"} // sorted
	if len(got) != len(want) {
		t.Fatalf("missingInputs() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("missingInputs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMissingInputsErrorNamesTheFix(t *testing.T) {
	declared := map[string]manifestInput{
		"STRIPE_KEY": {
			Description: "Stripe secret key",
			Example:     "sk_live_123",
			Required:    true,
		},
	}
	err := missingInputsError("api", declared, []string{"STRIPE_KEY"})
	msg := err.Error()

	// The error has to carry all three: what is missing, what it is for, and
	// how to supply it. An error that states a problem without stating the
	// fix just moves the work to the reader.
	for _, want := range []string{"STRIPE_KEY", "Stripe secret key", "sk_live_123", "qooq watch set-input api"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
}

func TestApplyInputsPrefersValueOverDefault(t *testing.T) {
	scope := newInterpolationScope(processEnvOnlyResolver())
	scope.set("host", "192.168.1.50")

	declared := map[string]manifestInput{
		"PUBLIC_HOST": {Default: "${host}"},
		"OVERRIDDEN":  {Default: "fallback"},
		"OPTIONAL":    {},
	}
	values := inputValues{"OVERRIDDEN": "explicit"}

	if err := applyInputs(scope, declared, values); err != nil {
		t.Fatalf("applyInputs: %v", err)
	}

	cases := map[string]string{
		"input.PUBLIC_HOST": "192.168.1.50", // default, expanded through the scope
		"input.OVERRIDDEN":  "explicit",     // supplied value wins
		"input.OPTIONAL":    "",             // declared but unset resolves to empty
	}
	for ref, want := range cases {
		got, ok := scope.resolve(ref)
		if !ok {
			t.Errorf("%s did not resolve", ref)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", ref, got, want)
		}
	}

	// An undeclared input is an unknown reference, which is what makes the
	// declaration block authoritative rather than decorative.
	if _, ok := scope.resolve("input.NEVER_DECLARED"); ok {
		t.Error("an undeclared input must not resolve")
	}
}

func TestInputRoundTripEncryptsOnlySecrets(t *testing.T) {
	w := &Watcher{secretKey: "test-key"}
	declared := map[string]manifestInput{
		"PUBLIC_HOST": {},
		"STRIPE_KEY":  {Secret: true},
	}
	values := inputValues{"PUBLIC_HOST": "192.168.1.50", "STRIPE_KEY": "sk_live_123"}

	encoded, err := w.storeInputValues(values, declared)
	if err != nil {
		t.Fatalf("storeInputValues: %v", err)
	}
	// The whole point of the secret flag: the plaintext must not be sitting
	// in the database where a backup or a stray `strings` would reveal it.
	if strings.Contains(encoded, "sk_live_123") {
		t.Errorf("secret value stored in plaintext:\n%s", encoded)
	}
	if !strings.Contains(encoded, "192.168.1.50") {
		t.Errorf("non-secret value should stay readable:\n%s", encoded)
	}

	back, err := w.loadInputValues(&Target{InputValues: encoded})
	if err != nil {
		t.Fatalf("loadInputValues: %v", err)
	}
	for name, want := range values {
		if back[name] != want {
			t.Errorf("round trip %s = %q, want %q", name, back[name], want)
		}
	}
}

func TestInputResponsesMaskSecrets(t *testing.T) {
	w := &Watcher{secretKey: "test-key"}
	declared := map[string]manifestInput{
		"PUBLIC_HOST": {Description: "browser address"},
		"STRIPE_KEY":  {Secret: true, Required: true},
		"UNSET":       {Required: true},
	}
	encoded, err := w.storeInputValues(inputValues{
		"PUBLIC_HOST": "192.168.1.50",
		"STRIPE_KEY":  "sk_live_123",
	}, declared)
	if err != nil {
		t.Fatalf("storeInputValues: %v", err)
	}

	resp := (&Target{DeclaredInputs: mustJSON(t, declared), InputValues: encoded}).toResponse()

	byName := map[string]InputResponse{}
	for _, in := range resp.Inputs {
		byName[in.Name] = in
	}
	if got := byName["STRIPE_KEY"]; got.Value != maskedSecret || !got.Set {
		t.Errorf("STRIPE_KEY = %+v, want masked and set", got)
	}
	if got := byName["PUBLIC_HOST"]; got.Value != "192.168.1.50" {
		t.Errorf("PUBLIC_HOST value = %q, want it echoed back", got.Value)
	}
	if got := byName["UNSET"]; got.Set || got.Value != "" {
		t.Errorf("UNSET = %+v, want unset with no value", got)
	}
	if len(resp.MissingInputs) != 1 || resp.MissingInputs[0] != "UNSET" {
		t.Errorf("MissingInputs = %v, want [UNSET]", resp.MissingInputs)
	}
}

func TestBuildBlockedCoversNeedsInput(t *testing.T) {
	// A target waiting on an input has not deployed. Anything gating on
	// "did this work" must see it as blocked, or a stack reads it as success
	// and marches on to deploy things that depend on it.
	if !buildBlocked(BuildStatusNeedsInput) {
		t.Error("needs_input must count as blocked")
	}
	if !buildBlocked(BuildStatusFailed) {
		t.Error("failed must count as blocked")
	}
	if buildBlocked(BuildStatusSuccess) {
		t.Error("success must not count as blocked")
	}
}

func TestSecretReferenceResolvesFromEnv(t *testing.T) {
	t.Setenv("QOOQ_SECRET_STRIPE", "sk_live_123")
	scope := newInterpolationScope(processEnvOnlyResolver())

	got, err := scope.expand("key=${secret.STRIPE}")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got != "key=sk_live_123" {
		t.Errorf("expand = %q, want key=sk_live_123", got)
	}

	// An unset secret is an unknown reference, not an empty string: an app
	// deployed with a blank API key fails far from its cause.
	if _, err := scope.expand("${secret.NOT_SET}"); err == nil {
		t.Error("an unset secret must not expand silently")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

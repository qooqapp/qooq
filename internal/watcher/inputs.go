package watcher

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// errNeedsInput wraps a pipeline error caused solely by missing input values,
// so the recording layer can mark the target needs_input instead of failed.
// It is a distinct type rather than a status flag because the decision belongs
// where the cause is known — deep inside the pipeline — and the recording
// happens at the top.
type errNeedsInput struct {
	err error
	// waitingOn names the components this deploy is waiting to exist, when
	// that is what it is blocked on; see Target.WaitingOn.
	waitingOn []string
}

func (e errNeedsInput) Error() string { return e.err.Error() }
func (e errNeedsInput) Unwrap() error { return e.err }

// needsInput marks err as a missing-input condition rather than a failure.
func needsInput(err error) error { return errNeedsInput{err: err} }

// waitingOnComponents marks err as needs_input because the components named
// are not deployed yet.
func waitingOnComponents(err error, names []string) error {
	return errNeedsInput{err: err, waitingOn: names}
}

// buildBlocked reports whether a build status means the target is not
// currently deployed from its latest source — failed outright, or waiting on
// an input. Callers that gate on "did this deploy work" must use this rather
// than comparing to BuildStatusFailed, or a target blocked on a missing value
// reads as a success.
func buildBlocked(status string) bool {
	return status == BuildStatusFailed || status == BuildStatusNeedsInput
}

// inputValues is the decoded form of Target.InputValues.
type inputValues map[string]string

// loadInputValues decodes the operator-supplied values, decrypting any that
// were stored encrypted. A value that cannot be decrypted (the secret key was
// changed or removed) is an error rather than a silent blank: deploying an app
// with an empty API key produces a failure far from its cause.
func (w *Watcher) loadInputValues(t *Target) (inputValues, error) {
	if strings.TrimSpace(t.InputValues) == "" {
		return inputValues{}, nil
	}
	var stored map[string]string
	if err := json.Unmarshal([]byte(t.InputValues), &stored); err != nil {
		return nil, fmt.Errorf("stored input values are corrupt: %w", err)
	}
	out := make(inputValues, len(stored))
	for name, raw := range stored {
		v, err := decryptCredential(raw, w.secretKey)
		if err != nil {
			return nil, fmt.Errorf("input %s: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// storeInputValues encodes values for persistence, encrypting the ones whose
// declaration marks them secret. Inputs with no declaration are encrypted too:
// a value supplied before the manifest was ever read might be a secret, and
// encrypting a non-secret costs nothing.
func (w *Watcher) storeInputValues(values inputValues, declared map[string]manifestInput) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	out := make(map[string]string, len(values))
	for name, v := range values {
		decl, known := declared[name]
		if !known || decl.Secret {
			enc, err := encryptCredential(v, w.secretKey)
			if err != nil {
				return "", fmt.Errorf("input %s: %w", name, err)
			}
			out[name] = enc
			continue
		}
		out[name] = v
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// decodeDeclaredInputs reads back the declarations cached on the target.
func decodeDeclaredInputs(raw string) (map[string]manifestInput, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out map[string]manifestInput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("stored input declarations are corrupt: %w", err)
	}
	return out, nil
}

// storedPresence adapts the raw stored map (values possibly still encrypted)
// to what missingInputs needs, which is only whether each name has a value.
// Using the raw map avoids decrypting just to answer "is it set".
func storedPresence(stored map[string]string) inputValues {
	out := make(inputValues, len(stored))
	for k, v := range stored {
		out[k] = v
	}
	return out
}

// missingInputs lists the declared inputs that are required, have no default,
// and have no value — sorted, so the message is stable across runs.
func missingInputs(declared map[string]manifestInput, values inputValues) []string {
	var missing []string
	for name, decl := range declared {
		if !decl.Required || decl.Default != "" {
			continue
		}
		if v, ok := values[name]; ok && v != "" {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	return missing
}

// missingInputsError renders the "you need to supply these" message. It names
// the command that supplies them, because an error that states a problem
// without stating the fix just moves the work to the reader.
func missingInputsError(targetName string, declared map[string]manifestInput, missing []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "missing required input")
	if len(missing) > 1 {
		b.WriteString("s")
	}
	b.WriteString(": ")
	b.WriteString(strings.Join(missing, ", "))
	for _, name := range missing {
		decl := declared[name]
		fmt.Fprintf(&b, "\n  %s", name)
		if decl.Description != "" {
			fmt.Fprintf(&b, " — %s", decl.Description)
		}
		if decl.Example != "" {
			fmt.Fprintf(&b, " (e.g. %s)", decl.Example)
		}
	}
	fmt.Fprintf(&b, "\nsupply with: qooq watch set-input %s %s=<value>", targetName, missing[0])
	return fmt.Errorf("%s", b.String())
}

// inputResponses renders the declared inputs for the API, in name order.
//
// It deliberately does not decrypt: it only needs to know whether a value is
// set, and reporting a secret's plaintext would defeat storing it encrypted.
// A non-secret value is echoed back so an operator can see what a target is
// actually deploying with — unless it happens to be stored encrypted (it was
// supplied before the manifest declared it non-secret), in which case saying
// "set" is more honest than printing ciphertext.
func inputResponses(declared map[string]manifestInput, stored map[string]string) []InputResponse {
	if len(declared) == 0 && len(stored) == 0 {
		return nil
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	// Values with no declaration still surface: an input removed from the
	// manifest but still stored is worth seeing rather than silently keeping.
	for name := range stored {
		if _, ok := declared[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	out := make([]InputResponse, 0, len(names))
	for _, name := range names {
		decl := declared[name]
		raw, isSet := stored[name]
		isSet = isSet && raw != ""
		resp := InputResponse{
			Name:        name,
			Description: decl.Description,
			Example:     decl.Example,
			Default:     decl.Default,
			Required:    decl.Required,
			Secret:      decl.Secret,
			Set:         isSet,
		}
		switch {
		case !isSet:
		case decl.Secret || isEncrypted(raw):
			resp.Value = maskedSecret
		default:
			resp.Value = raw
		}
		out = append(out, resp)
	}
	return out
}

// applyInputs registers ${input.NAME} for every declared input, resolving each
// to the supplied value or, failing that, its default. Defaults are expanded
// through the same scope, so `default: ${host}` works.
//
// Only declared inputs are registered. A ${input.X} with no declaration is an
// unknown reference and fails, which is what makes the declaration block
// authoritative rather than decorative.
func applyInputs(scope *interpolationScope, declared map[string]manifestInput, values inputValues) error {
	// Sorted for deterministic error reporting when several defaults are bad.
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		decl := declared[name]
		if v, ok := values[name]; ok && v != "" {
			scope.set("input."+name, v)
			continue
		}
		if decl.Default == "" {
			// Not required (missingInputs would have caught that) and not
			// supplied: register it as empty so a manifest that references an
			// optional input still resolves.
			scope.set("input."+name, "")
			continue
		}
		expanded, err := scope.expand(decl.Default)
		if err != nil {
			return fmt.Errorf("inputs.%s.default: %w", name, err)
		}
		scope.set("input."+name, expanded)
	}
	return nil
}

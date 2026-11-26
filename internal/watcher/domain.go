package watcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidDomain is returned for a hostname that could not be routed.
var ErrInvalidDomain = errors.New("invalid domain")

// validateDomain accepts a hostname made of DNS labels, at most 253 bytes in
// all. The empty string is valid and means "no domain".
//
// Checked here rather than left to the proxy: a bad name stored in a
// deploy_spec is read by every later build, and the proxy's complaint about it
// would surface minutes later and far from the request that wrote it.
func validateDomain(d string) error {
	if d == "" {
		return nil
	}
	if len(d) > 253 {
		return fmt.Errorf("%w: longer than 253 characters", ErrInvalidDomain)
	}
	for _, label := range strings.Split(d, ".") {
		if !validDNSLabel(label) {
			return fmt.Errorf("%w: %q is not a hostname (letters, digits and '-' in labels of 1 to 63 characters, not starting or ending with '-')", ErrInvalidDomain, d)
		}
	}
	return nil
}

func validDNSLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		ok := c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !ok {
			return false
		}
	}
	return true
}

// setTargetDomain writes domain into the target's deploy_spec and schedules a
// rebuild so the new address is routed right away.
//
// Only the "domain" key is touched: every other field is carried through as the
// raw JSON it was stored as, so nothing the operator set can change by being
// round-tripped through a struct that does not know it.
//
// Same locking shape as setTargetBranch, for the same reason: an in-flight build
// must finish, and clear force_rebuild, before this sets it. An unchanged value
// rebuilds nothing — a form submitted twice is not a request to restart the app.
func (w *Watcher) setTargetDomain(name, domain string) (t *Target, changed bool, err error) {
	if err := validateDomain(domain); err != nil {
		return nil, false, err
	}

	unlock := w.lockTarget(name)
	defer unlock()

	t, err = getTargetByName(w.db, name)
	if err != nil {
		return nil, false, err
	}
	if t.StackName != "" {
		return nil, false, fmt.Errorf("%w %q: the stack writes its own deploy_spec back on every sync",
			ErrTargetStackOwned, t.StackName)
	}

	fields := map[string]json.RawMessage{}
	if strings.TrimSpace(t.DeploySpec) != "" {
		if err := json.Unmarshal([]byte(t.DeploySpec), &fields); err != nil {
			return nil, false, fmt.Errorf("deploy_spec is not a JSON object: %w", err)
		}
	}
	var current string
	if raw, ok := fields["domain"]; ok {
		_ = json.Unmarshal(raw, &current)
	}
	if current == domain {
		return t, false, nil
	}
	if domain == "" {
		delete(fields, "domain")
	} else {
		raw, _ := json.Marshal(domain)
		fields["domain"] = raw
	}
	spec, err := json.Marshal(fields)
	if err != nil {
		return nil, false, err
	}

	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(map[string]any{
		"deploy_spec":   string(spec),
		"force_rebuild": true,
	}).Error; err != nil {
		return nil, false, err
	}
	if err := triggerNow(w.db, name); err != nil {
		return nil, false, err
	}
	t, err = getTargetByName(w.db, name)
	return t, true, err
}

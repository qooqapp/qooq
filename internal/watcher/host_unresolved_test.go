package watcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// ${host} comes from qooqd's capacity endpoint, which reads the host and does
// not touch docker — so stopping docker does not exercise this; qooqd itself
// has to be down (E1). Then the failure is not the repository's, and the
// message must not call ${host} an unknown reference while listing host as
// available: that sent users to edit a manifest that was fine.
func TestHostThatCannotBeAskedForIsNotTheManifestsFault(t *testing.T) {
	w := &Watcher{client: client.New("http://127.0.0.1:1"), log: zap.NewNop()} // nothing listens
	run := func(env string) error {
		mf := &manifest{App: "hostapp"}
		mf.Run.Env = map[string]string{"BASE": env}
		tg := &Target{Name: "acme--prod_hostapp", ComponentName: "hostapp", DeploySpec: `{"namespace":"acme--prod"}`}
		_, err := w.applyRunConfig(context.Background(), tg, mf, deploySpecFields{Namespace: "acme--prod"}, t.TempDir(), "abc1234")
		return err
	}

	err := run("http://${host}:8080")
	if err == nil {
		t.Fatal("${host} resolved with no qooqd to ask")
	}
	if !isNotTheRepository(err) {
		t.Errorf("counted against the repository: %v", err)
	}
	if strings.Contains(err.Error(), "unknown reference") || !strings.Contains(err.Error(), "could not be asked for") ||
		strings.Count(err.Error(), "qooq.yaml") > 1 {
		t.Errorf("message = %q, want what happened, not an unknown reference", err.Error())
	}

	// An ordinary mistake in the manifest is still its own: counted, and
	// worded as before.
	err = run("http://${nope}:8080")
	if err == nil || isNotTheRepository(err) || !strings.Contains(err.Error(), "unknown reference ${nope}") {
		t.Errorf("an unknown reference was not reported as the manifest's: %v", err)
	}
}

// Two wrapping layers saying the same thing reached the user three times
// ("build failed: build failed:", the low-disk sentence, "qooq.yaml:
// qooq.yaml:"). The one place every message is stored collapses it.
func TestARepeatedPrefixIsSaidOnce(t *testing.T) {
	for in, want := range map[string]string{
		"qooq.yaml: qooq.yaml: ${host} cannot be resolved": "qooq.yaml: ${host} cannot be resolved",
		"build failed: build failed: exit 1":               "build failed: exit 1",
		"qooq.yaml: run.env: BASE: unknown reference":      "qooq.yaml: run.env: BASE: unknown reference",
		"a: b: a: b": "a: b: a: b",
	} {
		if got := collapseRepeatedPrefix(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	w := newStatusWatcher(t)
	tg := &Target{Name: "web", ComponentName: "web"}
	saveTarget(t, w, tg)
	w.recordFailure(tg, errors.New("qooq.yaml: qooq.yaml: no"))
	if got := getTargetByID(t, w.db, tg.ID).LastBuildError; got != "qooq.yaml: no" {
		t.Errorf("stored %q", got)
	}

	// The other place a message is stored. Testing the helper alone left this
	// call unguarded: removing it kept every test green.
	tg2 := &Target{Name: "web2", ComponentName: "web2"}
	saveTarget(t, w, tg2)
	w.recordNeedsInput(tg2, "qooq.yaml: qooq.yaml: needs API_KEY")
	got := getTargetByID(t, w.db, tg2.ID)
	if got.LastBuildStatus != BuildStatusNeedsInput || got.LastBuildError != "qooq.yaml: needs API_KEY" {
		t.Errorf("needs input stored %q / %q", got.LastBuildStatus, got.LastBuildError)
	}
}

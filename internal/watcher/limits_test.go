package watcher

import (
	"strings"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

func sharedNodeLimits(t *testing.T) containerLimits {
	t.Helper()
	l, err := newContainerLimits(0.5, "512Mi", 2, "2Gi")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The case that was unlimited: no qooq.yaml, so nothing asked for a limit.
func TestAContainerThatAskedForNothingGetsTheDefault(t *testing.T) {
	cpu, mem, err := sharedNodeLimits(t).apply(0, 0)
	if err != nil || cpu != 0.5 || mem != 512<<20 {
		t.Errorf("apply(0, 0) = %g, %d, %v; want 0.5, %d", cpu, mem, err, 512<<20)
	}
}

func TestWhatAManifestAskedForWithinTheMaximumIsKept(t *testing.T) {
	cpu, mem, err := sharedNodeLimits(t).apply(1.5, 1<<30)
	if err != nil || cpu != 1.5 || mem != 1<<30 {
		t.Errorf("apply(1.5, 1Gi) = %g, %d, %v", cpu, mem, err)
	}
	// Exactly the maximum is allowed.
	if _, _, err := sharedNodeLimits(t).apply(2, 2<<30); err != nil {
		t.Errorf("the maximum itself was refused: %v", err)
	}
}

// Refused with the numbers, not clamped: an app that was quietly given less
// memory than it asked for fails somewhere far from the line that asked.
func TestAskingForMoreThanTheMaximumIsRefusedWithTheNumbers(t *testing.T) {
	_, _, err := sharedNodeLimits(t).apply(4, 0)
	if err == nil || !strings.Contains(err.Error(), "4 cores") || !strings.Contains(err.Error(), "at most 2") {
		t.Errorf("cpu over max: %v", err)
	}
	_, _, err = sharedNodeLimits(t).apply(0, 3<<30)
	if err == nil || !strings.Contains(err.Error(), "3Gi") || !strings.Contains(err.Error(), "at most 2Gi") {
		t.Errorf("memory over max: %v", err)
	}
}

// Nothing configured is the self-hosted install, and stays exactly as before.
func TestNoLimitsConfiguredChangesNothing(t *testing.T) {
	var l containerLimits
	cpu, mem, err := l.apply(0, 0)
	if err != nil || cpu != 0 || mem != 0 {
		t.Errorf("unset limits gave %g, %d, %v", cpu, mem, err)
	}
	cpu, mem, err = l.apply(64, 512<<30)
	if err != nil || cpu != 64 || mem != 512<<30 {
		t.Errorf("unset limits refused or changed %g, %d: %v", cpu, mem, err)
	}
}

func TestBadLimitSettingsStopTheWatcherFromStarting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dCPU, mCPU float64
		dMem, mMem string
	}{
		{"memory typo", 0.5, 2, "512Mb?", "2Gi"},
		{"default above max cpu", 4, 2, "", ""},
		{"default above max memory", 0, 0, "4Gi", "2Gi"},
		{"negative", -1, 0, "", ""},
	} {
		if _, err := newContainerLimits(tc.dCPU, tc.dMem, tc.mCPU, tc.mMem); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// Through buildDeployRequest, the one place every deploy path builds its
// request: the default reaches what is sent to qooqd.
func TestTheDeployRequestCarriesTheDefault(t *testing.T) {
	w := giveUpWatcher(t)
	w.limits = sharedNodeLimits(t)
	tg := mkTarget(t, w, "app")

	req, err := w.buildDeployRequest(tg, "app:abc1234", deploySpecFields{})
	if err != nil {
		t.Fatal(err)
	}
	if req.CPULimit != 0.5 || req.MemLimitBytes != 512<<20 {
		t.Errorf("request limits = %g, %d; want 0.5, %d", req.CPULimit, req.MemLimitBytes, 512<<20)
	}

	if _, err := w.buildDeployRequest(tg, "app:abc1234", deploySpecFields{CPULimit: 8}); err == nil {
		t.Error("a manifest over the maximum was deployed")
	}
}

// The route and encoded tag are dropped, the daemon's words kept; a timeout
// says what to look for. The client error stays in the chain.
func TestABuildFailureReadsLikeOne(t *testing.T) {
	timeout := buildFailure(&client.APIError{Method: "POST", Path: "/v1/images/build?dockerfile=Dockerfile&tag=x%3Aabc", StatusCode: 504,
		Message: "build exceeded this daemon's build timeout after 40s"})
	if s := timeout.Error(); strings.Contains(s, "/v1/") || !strings.Contains(s, "(40s)") || !strings.Contains(s, "never finishes") {
		t.Errorf("timeout = %q", s)
	}
	if client.StatusCode(timeout) != 504 {
		t.Error("the status was lost from the chain")
	}
	plain := buildFailure(&client.APIError{Method: "POST", Path: "/v1/images/build?tag=x", StatusCode: 500, Message: "The command '/bin/sh -c npm ci' returned a non-zero code: 1"})
	if s := plain.Error(); s != "build failed: The command '/bin/sh -c npm ci' returned a non-zero code: 1" {
		t.Errorf("plain = %q", s)
	}
}

// The daemon masks a 5xx as "internal error"; the build's own last line is the
// reason, and the message uses it.
func TestAMaskedBuildFailureUsesTheBuildsLastLine(t *testing.T) {
	err := buildFailure(&client.APIError{StatusCode: 500, Message: "internal error",
		BuildLog: "Step 2/3 : RUN exit 3\n ---> Running in 868dc51449bd\n\n\x1b[31mThe command '/bin/sh -c exit 3' returned a non-zero code: 3\x1b[0m\n\n"})
	if got := err.Error(); got != "build failed: The command '/bin/sh -c exit 3' returned a non-zero code: 3" {
		t.Errorf("message = %q", got)
	}
}

func TestABuildKilledFor137SaysItWasProbablyMemory(t *testing.T) {
	err := buildFailure(&client.APIError{StatusCode: 500, Message: "internal error",
		BuildLog: "490 MB\nThe command '/bin/sh -c python /mem.py' returned a non-zero code: 137\n"})
	if !strings.Contains(err.Error(), "non-zero code: 137 — 137 means the step was killed") || !strings.Contains(err.Error(), "memory") {
		t.Errorf("message = %q", err.Error())
	}
}

// A full machine is not the repository's fault: not counted toward a pause.
func TestALowDiskRefusalIsNotTheRepositorysFault(t *testing.T) {
	err := buildFailure(&client.APIError{StatusCode: 507, Message: "this server is low on disk space, so builds are paused (9% free, builds need 15%)"})
	if !isNotTheRepository(err) {
		t.Error("a low-disk refusal would count toward pausing the target")
	}
	if !strings.Contains(err.Error(), "low on disk") || !strings.Contains(err.Error(), "on its own") {
		t.Errorf("message = %q", err.Error())
	}
	// Said once, with the figures kept (it used to repeat qooqd's sentence
	// inside its own).
	if n := strings.Count(err.Error(), "low on disk"); n != 1 {
		t.Errorf("the sentence appears %d times: %q", n, err.Error())
	}
	if !strings.Contains(err.Error(), "(9% free, builds need 15%)") {
		t.Errorf("the figures are gone: %q", err.Error())
	}
	// A message of another shape is kept whole rather than lost.
	if got := lowDiskFigures("disk nearly full"); got != "disk nearly full" {
		t.Errorf("lowDiskFigures of an unknown shape = %q", got)
	}
}

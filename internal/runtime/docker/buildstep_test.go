package docker

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// streamOf builds the JSON progress stream docker sends, so the parser is
// exercised through the same shape it meets in production rather than against a
// hand-made string.
func streamOf(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		enc, _ := json.Marshal(map[string]string{"stream": l})
		b.Write(enc)
		b.WriteByte('\n')
	}
	return b.String()
}

// The id of the step in flight is the only handle there is on a RUN that
// BuildCancel cannot interrupt. It comes from the builder's own announcement.
func TestBuildStreamTracksTheStepContainer(t *testing.T) {
	var step string
	var log bytes.Buffer
	_, err := drainBuildStreamTracking(strings.NewReader(streamOf(
		"Step 1/3 : FROM alpine:3.20\n",
		" ---> bf8527eb54c3\n",
		"Step 2/3 : RUN echo one\n",
		" ---> Running in 18b4c94eb8bb\n",
		" ---> Removed intermediate container 18b4c94eb8bb\n",
		"Step 3/3 : RUN sleep 600\n",
		" ---> Running in bd652c0466b3\n",
	)), &log, &step)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// The LAST one: that is the step still running when a build is abandoned.
	// Keeping the first would remove a container the builder already cleaned up
	// and leave the one actually burning the machine.
	if step != "bd652c0466b3" {
		t.Fatalf("step container = %q, want the most recent one", step)
	}
	if !strings.Contains(log.String(), "Step 3/3") {
		t.Errorf("tracking the container swallowed the log: %q", log.String())
	}
}

// A build that never reaches a RUN has no step to kill, and must not offer a
// container id that would remove something else.
func TestBuildStreamHasNoStepContainerBeforeAnyRun(t *testing.T) {
	var step string
	if _, err := drainBuildStreamTracking(strings.NewReader(streamOf(
		"Step 1/1 : FROM alpine:3.20\n",
		" ---> bf8527eb54c3\n",
	)), nil, &step); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if step != "" {
		t.Fatalf("step container = %q, want empty", step)
	}
}

// Lines that merely mention the words must not be mistaken for the
// announcement — the value is fed straight into a forced container removal.
func TestBuildStreamIgnoresLinesThatAreNotTheAnnouncement(t *testing.T) {
	var step string
	if _, err := drainBuildStreamTracking(strings.NewReader(streamOf(
		"Step 1/2 : FROM alpine\n",
		"Running in production is not what this says\n",
		" ---> Running in notahexid\n",
		"echo ' ---> Running in deadbeefcafe' > /tmp/x\n",
	)), nil, &step); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if step != "" {
		t.Fatalf("step container = %q, want empty — none of those lines is the builder's own", step)
	}
}

// The non-tracking wrapper still behaves, since the push path uses it.
func TestDrainBuildStreamStillReportsTheImageID(t *testing.T) {
	enc, _ := json.Marshal(map[string]any{"aux": map[string]string{"ID": "sha256:abc"}})
	id, err := drainBuildStream(strings.NewReader(string(enc)+"\n"), nil)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if id != "sha256:abc" {
		t.Fatalf("image id = %q", id)
	}
}

package core

import (
	"context"
	"strings"
	"testing"
)

// TestRecoverRestartPanicDoesNotPropagate proves the recover boundary around
// a scheduled restart (time.AfterFunc's callback in EvaluateRestart) actually
// stops a panic from escaping: without it, a panic inside performRestart runs
// on a goroutine with nothing else on the stack to catch it, which crashes the
// whole qooqd process over one component's bad luck.
func TestRecoverRestartPanicDoesNotPropagate(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)
	comp := mustDeploy(t, s, "web")

	escaped := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				escaped = true
			}
		}()
		s.recoverRestartPanic(context.Background(), comp.ID, RestartReasonCrash, func() {
			panic("boom")
		})
	}()

	if escaped {
		t.Fatal("panic escaped recoverRestartPanic; it must be recovered here, not left to whatever (if anything) is above time.AfterFunc's callback on the stack")
	}
}

// TestRecoverRestartPanicRecordsEvent proves the second half of the
// requirement: a recovered panic must not vanish silently. It has to leave a
// durable, visible trace on the component (a panic_recovered event, the same
// mechanism NoteHealthUnreachable and restart_exhausted already use) so an
// operator looking at `qooq component get` finds out it happened, not just
// whoever happened to be tailing the daemon log at that moment.
func TestRecoverRestartPanicRecordsEvent(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)
	comp := mustDeploy(t, s, "web")

	s.recoverRestartPanic(context.Background(), comp.ID, RestartReasonHealth, func() {
		panic("boom")
	})

	events, err := st.ListEvents(comp.ID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found *Event
	for i := range events {
		if events[i].Type == "panic_recovered" {
			found = &events[i]
		}
	}
	if found == nil {
		t.Fatalf("no panic_recovered event recorded; events: %+v", events)
	}
	if !strings.Contains(found.Message, "boom") {
		t.Errorf("panic_recovered message = %q, want it to mention the recovered value", found.Message)
	}
	if !strings.Contains(found.Message, "health") {
		t.Errorf("panic_recovered message = %q, want it to say which restart reason was in progress", found.Message)
	}
}

// TestRecoverRestartPanicOnUnknownComponentDoesNotError confirms that a panic
// recovered for a component id that no longer resolves (deleted between
// scheduling and firing) is a no-op on the store side rather than an error —
// NotePanicRecovered already handles "component not found" by doing nothing,
// this just pins that recoverRestartPanic does not additionally panic or log
// in a way that breaks the recovery.
func TestRecoverRestartPanicOnUnknownComponentDoesNotError(t *testing.T) {
	rt := newFakeRuntime()
	st := newFakeStore()
	s := newTestService(rt, st)

	escaped := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				escaped = true
			}
		}()
		s.recoverRestartPanic(context.Background(), "does-not-exist", RestartReasonDrift, func() {
			panic("boom")
		})
	}()
	if escaped {
		t.Fatal("panic escaped recoverRestartPanic for an unresolvable component id")
	}
}

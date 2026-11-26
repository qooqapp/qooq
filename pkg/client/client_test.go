package client

import (
	"context"
	"testing"
	"time"
)

func TestStreamDeadline(t *testing.T) {
	t.Run("caps an unbounded context", func(t *testing.T) {
		c := New("http://x", WithStreamTimeout(5*time.Minute))
		ctx, cancel := c.streamDeadline(context.Background())
		defer cancel()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected a deadline to be imposed on an unbounded context")
		}
		if d := time.Until(dl); d <= 0 || d > 5*time.Minute+time.Second {
			t.Fatalf("deadline out of expected range: %v", d)
		}
	})

	t.Run("caller deadline wins", func(t *testing.T) {
		c := New("http://x") // default 30m cap
		caller, cancelCaller := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelCaller()
		ctx, cancel := c.streamDeadline(caller)
		defer cancel()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected the caller's deadline to be preserved")
		}
		// Must reflect the caller's 2s, not the 30m default cap.
		if d := time.Until(dl); d > 5*time.Second {
			t.Fatalf("caller deadline was overridden by the cap: %v", d)
		}
	})

	t.Run("zero timeout disables the cap", func(t *testing.T) {
		c := New("http://x", WithStreamTimeout(0))
		ctx, cancel := c.streamDeadline(context.Background())
		defer cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("expected no deadline when the cap is disabled")
		}
	})
}

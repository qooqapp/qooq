package core

import "sync"

// tailBuffer keeps the last n bytes written to it and discards the rest.
//
// Used for build output, where the interesting part is at the end: a build
// fails on its last step, and the last screenful is almost always the whole
// diagnosis. A head-limited buffer would reliably keep "Step 1/9 : FROM
// golang" and lose the compiler error.
//
// It never fails and never blocks. A build must not be affected by what
// happens to its log — recording is an observability concern, and letting it
// turn into an outage would be a poor trade.
type tailBuffer struct {
	mu sync.Mutex
	// buf holds at most max bytes; once full, writes shift it left.
	buf []byte
	max int
	// dropped records whether anything was discarded, so the reader can be
	// told the log is a tail rather than the whole thing. Silently returning a
	// truncated log looks like a build that stopped for no reason.
	dropped bool
}

func newTailBuffer(max int) *tailBuffer {
	if max <= 0 {
		max = 1
	}
	return &tailBuffer{max: max}
}

// Write is safe to call from another goroutine: the adapter decides where it
// reads the stream from, and this must not constrain that choice.
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(p)
	if n >= b.max {
		// This single write already exceeds the budget; keep only its tail.
		copy(b.buf[:0], p[n-b.max:])
		b.buf = append(b.buf[:0], p[n-b.max:]...)
		b.dropped = true
		return n, nil
	}

	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = b.buf[over:]
		b.dropped = true
	}
	return n, nil
}

// String returns the retained output, prefixed with a note when earlier output
// was discarded.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dropped {
		return string(b.buf)
	}
	return "… (earlier output omitted; showing the last part of the build)\n" + string(b.buf)
}

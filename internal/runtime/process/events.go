//go:build unix

package process

import (
	"context"
	"sync"

	"github.com/qooqapp/qooq/internal/runtime"
)

// subBuffer is the per-subscriber channel depth. Only internal/monitor is
// expected to subscribe (once, for the process lifetime), but the bus
// supports any number of subscribers for robustness.
const subBuffer = 256

// eventBus fans out lifecycle events to Events() subscribers. Events are
// published from several independent goroutines (StartContainer,
// waitForExit, the adopted-process poller), so publish/subscribe must be
// safe for concurrent use without ever blocking the publisher.
type eventBus struct {
	mu   sync.Mutex
	subs map[chan runtime.Event]runtime.Filter
}

func newEventBus() *eventBus {
	return &eventBus{subs: make(map[chan runtime.Event]runtime.Filter)}
}

func (b *eventBus) subscribe(filter runtime.Filter) chan runtime.Event {
	ch := make(chan runtime.Event, subBuffer)
	b.mu.Lock()
	b.subs[ch] = filter
	b.mu.Unlock()
	return ch
}

func (b *eventBus) unsubscribe(ch chan runtime.Event) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// publish delivers ev to every subscriber whose filter labels match. It
// never blocks: a subscriber slow enough to fill its 256-deep buffer has the
// event dropped rather than stalling the wait/poller goroutine that called
// publish.
func (b *eventBus) publish(ev runtime.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, filter := range b.subs {
		if !labelsMatch(filter.Labels, ev.Labels) {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
}

// Events streams lifecycle events matching filter's labels until ctx is
// cancelled, at which point both channels are closed (errs first, carrying
// ctx.Err(), then out) — mirroring the docker adapter's contract.
func (a *Adapter) Events(ctx context.Context, filter runtime.Filter) (<-chan runtime.Event, <-chan error) {
	ch := a.bus.subscribe(filter)
	errs := make(chan error, 1)

	go func() {
		<-ctx.Done()
		a.bus.unsubscribe(ch)
		errs <- ctx.Err()
		close(errs)
		close(ch)
	}()

	return ch, errs
}

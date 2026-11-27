package watcher

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// waitCycle reports a loop of ${component.X.*} references that can never
// resolve: this target waits for a component whose own target is waiting
// (needs_input) for one that leads back here. It returns the loop as component
// names, starting and ending with this target's component, or nil.
//
// A reference to a component that is not deployed yet is normally an ordering
// problem that fixes itself, which is why it is needs_input and not a failure.
// A cycle is the one case where it does not: none of them can go first, and
// every one of them says "not deployed yet" forever. Saying so is the whole
// point; the status stays needs_input, because editing any one manifest in the
// loop is enough for the next sweep to get through.
//
// Only targets that already recorded what they are waiting for take part, so
// the first target of a loop to be tried is reported as merely waiting and the
// next one closes it. That is fine: needs_input is retried every sweep.
func (w *Watcher) waitCycle(self *Target, namespace string, pending []string) []string {
	if len(pending) == 0 || self.ComponentName == "" {
		return nil
	}
	var waiting []Target
	if err := w.db.Where("last_build_status = ? AND waiting_on <> ''", BuildStatusNeedsInput).Find(&waiting).Error; err != nil {
		w.log.Warn("could not check for circular component references", zap.String("target", self.Name), zap.Error(err))
		return nil
	}
	edges := map[string][]string{}
	for i := range waiting {
		o := &waiting[i]
		if o.ID == self.ID || namespaceOf(o) != namespace {
			continue
		}
		edges[o.ComponentName] = strings.Split(o.WaitingOn, ",")
	}
	edges[self.ComponentName] = pending
	return findCycle(edges, self.ComponentName)
}

// findCycle returns a path start → … → start through edges, or nil.
func findCycle(edges map[string][]string, start string) []string {
	visited := map[string]bool{}
	var path []string
	var walk func(n string) bool
	walk = func(n string) bool {
		path = append(path, n)
		for _, next := range edges[n] {
			if next == start {
				path = append(path, start)
				return true
			}
			if visited[next] {
				continue
			}
			visited[next] = true
			if walk(next) {
				return true
			}
		}
		path = path[:len(path)-1]
		return false
	}
	visited[start] = true
	if walk(start) {
		return path
	}
	return nil
}

func circularReferenceMessage(cycle []string, namespace string) string {
	return fmt.Sprintf("circular reference in namespace %q: %s. Each waits for the other to be deployed first, so none of them ever will. "+
		"For the first deploy, give one of them a fixed value instead of the ${component...} reference, deploy it, then put the reference back and redeploy",
		namespace, strings.Join(cycle, " -> "))
}

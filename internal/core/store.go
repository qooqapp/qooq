package core

import "io"

// Store is the persistence boundary. It is declared here (consumer-defined
// interface) so that core depends on an abstraction rather than on a concrete
// database package — internal/store/bbolt implements it. Missing keys yield
// ErrNotFound.
type Store interface {
	SaveComponent(c *Component) error
	GetComponent(id string) (*Component, error)
	ListComponents() ([]*Component, error)
	DeleteComponent(id string) error

	SaveGroup(g *Group) error
	GetGroup(id string) (*Group, error)
	ListGroups() ([]*Group, error)
	DeleteGroup(id string) error

	AppendEvent(componentID string, e Event) error
	ListEvents(componentID string) ([]Event, error)

	// Backup writes a consistent point-in-time snapshot of the entire store
	// to w. Safe to call on a live store. If sizeHint is non-nil it is called
	// exactly once, before any bytes are written, with the total snapshot size
	// — HTTP handlers use it to set Content-Length so a mid-stream failure is
	// detectable by the client instead of looking like a clean EOF.
	Backup(w io.Writer, sizeHint func(int64)) error

	Close() error
}

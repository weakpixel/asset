package asset

import "iter"

// EventKind says what happened to an asset.
type EventKind uint8

const (
	// EventLoaded: the asset was committed and is Ready.
	EventLoaded EventKind = iota
	// EventFailed: the asset failed; Event.Err says why.
	EventFailed
	// EventModified: hot reload replaced the value. The handle is the same;
	// Store.Version went up. Systems that derived data from the asset (a
	// collision grid, cached images) rebuild it.
	EventModified
	// EventUnloaded: the asset was unloaded and its handles are now stale.
	EventUnloaded
)

// Event is one thing that happened during an Update.
type Event struct {
	Kind EventKind
	ID   ID
	Key  Key
	Err  error // set for EventFailed
}

// Events is the list for one [Server.Update]. The same Events value (and its
// backing storage) is reused, so it is valid until the next Update.
type Events struct{ list []Event }

// Len is the number of events.
func (e *Events) Len() int { return len(e.list) }

// All yields every event in the order it happened.
func (e *Events) All() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for _, ev := range e.list {
			if !yield(ev) {
				return
			}
		}
	}
}

// Of yields the events of one asset type. Get the TypeID from [TypeIDOf].
func (e *Events) Of(t TypeID) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		for _, ev := range e.list {
			if ev.ID.Type == t && !yield(ev) {
				return
			}
		}
	}
}

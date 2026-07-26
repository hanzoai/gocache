package main

import "sync/atomic"

// Tier composes the two levels. Local disk answers first and answers most of
// the time; the shared tier only ever turns a miss into a hit. Remote may be
// nil, in which case this is exactly the cache the go command keeps for
// itself.
type Tier struct {
	disk   *Disk
	remote *Remote

	Local, Shared, Misses atomic.Int64
}

func (t *Tier) Get(a ID) (Entry, bool) {
	if e, ok := t.disk.Get(a); ok {
		t.Local.Add(1)
		return e, true
	}
	if t.remote == nil {
		t.Misses.Add(1)
		return Entry{}, false
	}
	out, body, ok := t.remote.Get(a)
	if !ok {
		t.Misses.Add(1)
		return Entry{}, false
	}
	// Promote into the local tier so the next build in this checkout, and the
	// rest of this one, never crosses the network for it again.
	e, err := t.disk.Put(a, out, body)
	if err != nil {
		t.Misses.Add(1)
		return Entry{}, false
	}
	t.Shared.Add(1)
	return e, true
}

func (t *Tier) Put(a, o ID, body []byte) (Entry, error) {
	e, err := t.disk.Put(a, o, body)
	if err != nil {
		return Entry{}, err
	}
	if t.remote != nil {
		t.remote.Offer(a, o, e.Path, e.Size)
	}
	return e, nil
}

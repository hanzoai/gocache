package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Remote is the shared tier, wrapped in the policy that keeps a build
// independent of it. Four rules, and every one of them exists because the go
// command treats this program as infrastructure: it aborts the build if we
// crash, hang, or answer nonsense.
//
//  1. Every operation has a deadline. A slow store costs a bounded amount of
//     time, never an unbounded one.
//  2. Consecutive failures trip a breaker. Once tripped it stays tripped for
//     the life of the process, so a store that is down costs at most
//     limit x deadline in total rather than that much per action.
//  3. Uploads go through a bounded queue and are dropped when it is full.
//     A put never waits on the network.
//  4. Credentials are resolved behind the handshake, and waiting for them is
//     bounded by the same deadline as any other operation. A wedged KMS is
//     just another unreachable shared tier.
//
// The result: with the shared tier unreachable, the build runs at exactly
// local-disk speed and produces exactly the same bytes.
type Remote struct {
	Policy

	// ready is closed once the session has been resolved, successfully or not.
	// session is written before the close and read only after it, so the close
	// is what publishes it.
	ready   chan struct{}
	session *session

	fails   atomic.Int64
	tripped atomic.Bool
	why     atomic.Pointer[string]

	// queue is guarded because a send on a closed channel panics, and a panic
	// in this program is a failed build. Offer holds it for reading, Drain for
	// writing, so the close can never overtake a send in flight. Puts are
	// already serialized by the protocol loop, so there is nothing to contend
	// over.
	qmu    sync.RWMutex
	closed bool
	queue  chan upload
	done   chan struct{}

	Hits, Misses, Fails, Uploads, Dropped atomic.Int64
	DownBytes, UpBytes                    atomic.Int64
}

// Policy is what the shared tier does not learn from the network: where its
// keyspace is, whether it may write, and how much time and space it is allowed.
type Policy struct {
	Prefix  string
	Write   bool
	Get     time.Duration
	Put     time.Duration
	MaxSize int64
	Workers int
	Limit   int64
}

// session is what the shared tier cannot start without and cannot get locally:
// somewhere to talk to, and the key that says which objects are ours. Both come
// from KMS, together, after the handshake.
type session struct {
	store   Store
	sealKey []byte
}

type upload struct {
	action ID
	output ID
	path   string
	size   int64
}

var errNoSession = errors.New("no credentials for the shared tier")

func NewRemote(p Policy) *Remote {
	r := &Remote{
		Policy: p,
		ready:  make(chan struct{}),
		queue:  make(chan upload, 256),
		done:   make(chan struct{}),
	}
	var wg sync.WaitGroup
	for range p.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range r.queue {
				r.upload(u)
			}
		}()
	}
	go func() { wg.Wait(); close(r.done) }()
	return r
}

// Open supplies what the shared tier has been waiting for, or the reason it
// will never arrive. Exactly one call, and it releases everything blocked on
// the gate either way: a failure to resolve credentials must not leave a build
// waiting.
func (r *Remote) Open(s *session, err error) {
	if err != nil {
		r.Disable(err)
	} else {
		r.session = s
	}
	close(r.ready)
}

// use returns the session, waiting no longer than the caller's deadline allows.
func (r *Remote) use(ctx context.Context) (*session, error) {
	select {
	case <-r.ready:
		if r.session == nil {
			return nil, errNoSession
		}
		return r.session, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Remote) key(a ID) string {
	s := a.String()
	b := s
	if len(s) >= 2 {
		b = s[:2]
	}
	return r.Prefix + "/" + b + "/" + s
}

func (r *Remote) ok() bool { return !r.tripped.Load() }

// Disable turns the shared tier off for the rest of the process. Setup that
// cannot complete -- no credentials, an unparseable address -- reports here
// once rather than failing every action in turn.
func (r *Remote) Disable(err error) {
	if r.tripped.CompareAndSwap(false, true) {
		s := err.Error()
		r.why.Store(&s)
	}
}

// Why reports what turned the shared tier off, for the statistics line. A
// developer whose cache stopped helping should be able to find out in one
// command rather than by reading this program.
func (r *Remote) Why() string {
	if s := r.why.Load(); s != nil {
		return *s
	}
	return ""
}

// pass records a healthy answer. A miss is a healthy answer: an empty shared
// cache must not look like a broken one.
func (r *Remote) pass() { r.fails.Store(0) }

func (r *Remote) trip(err error) {
	r.Fails.Add(1)
	if r.fails.Add(1) >= r.Limit {
		r.Disable(fmt.Errorf("%d consecutive failures, last: %w", r.Limit, err))
	}
}

// Get fetches an action result from the shared tier. Every failure mode,
// including a corrupt or forged object, returns a miss: the build then compiles
// the action locally, which is always correct.
func (r *Remote) Get(a ID) (ID, []byte, bool) {
	if !r.ok() {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.Policy.Get)
	defer cancel()

	s, err := r.use(ctx)
	if err != nil {
		r.trip(err)
		return nil, nil, false
	}

	body, err := s.store.Get(ctx, r.key(a))
	if errors.Is(err, ErrMiss) {
		r.pass()
		r.Misses.Add(1)
		return nil, nil, false
	}
	if err != nil {
		r.trip(err)
		return nil, nil, false
	}
	defer body.Close()

	// An object that fails its digest or its MAC counts against the failure
	// budget, the same as a transport error. That is deliberate: a store handing
	// out objects this cache did not produce is not healthy, and tripping early
	// bounds how many of them a build looks at. The build is correct either way,
	// because both paths compile locally.
	out, data, err := read(body, s.sealKey, a, r.MaxSize)
	if err != nil {
		r.trip(err)
		return nil, nil, false
	}
	r.pass()
	r.Hits.Add(1)
	r.DownBytes.Add(int64(len(data)))
	return out, data, true
}

// Offer hands an action result to the upload queue. It never blocks and never
// reports failure: an upload that does not happen costs a future cache miss,
// and a build that waits on an upload costs the developer.
func (r *Remote) Offer(a, o ID, path string, size int64) {
	if !r.Write || !r.ok() || size > r.MaxSize {
		return
	}
	r.qmu.RLock()
	defer r.qmu.RUnlock()
	if r.closed {
		return
	}
	select {
	case r.queue <- upload{a, o, path, size}:
	default:
		r.Dropped.Add(1)
	}
}

func (r *Remote) upload(u upload) {
	if !r.ok() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.Policy.Put)
	defer cancel()

	s, err := r.use(ctx)
	if err != nil {
		r.trip(err)
		return
	}

	// Three passes over a file that was written moments ago and is still in the
	// page cache: the MAC, then the request signature over header and body,
	// then the body itself. The signature covers the header, and the header
	// carries the MAC, so they cannot be computed in one pass. All of it runs
	// after the go command has been released.
	mac, err := sealFile(s.sealKey, u.action, u.output, u.size, u.path)
	if err != nil {
		// The file is gone or no longer the size the record claims. Nothing to
		// upload, and nothing broken about the shared tier.
		r.Dropped.Add(1)
		return
	}
	head := object(u.output, u.size, mac)
	sum, n, err := hashFile(head, u.path)
	if err != nil || n != u.size {
		r.Dropped.Add(1)
		return
	}
	f, err := os.Open(u.path)
	if err != nil {
		r.Dropped.Add(1)
		return
	}
	defer f.Close()

	total := int64(len(head)) + n
	err = s.store.Put(ctx, r.key(u.action), total, io.MultiReader(bytes.NewReader(head), f), sum)
	if err != nil {
		r.trip(err)
		return
	}
	r.pass()
	r.Uploads.Add(1)
	r.UpBytes.Add(total)
}

// Drain stops accepting uploads and waits for the queued ones, up to a
// deadline. It is called after the go command has already been released, so the
// wait delays nothing but this program's own exit.
func (r *Remote) Drain(d time.Duration) {
	r.qmu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.qmu.Unlock()

	select {
	case <-r.done:
	case <-time.After(d):
	}
}

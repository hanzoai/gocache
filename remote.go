package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Remote is the shared tier, wrapped in the policy that keeps a build
// independent of it. Three rules, and every one of them exists because the go
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
//
// The result: with the shared tier unreachable, the build runs at exactly
// local-disk speed and produces exactly the same bytes.
type Remote struct {
	store  Store
	prefix string
	write  bool

	getDeadline time.Duration
	putDeadline time.Duration
	maxSize     int64
	limit       int64

	fails   atomic.Int64
	tripped atomic.Bool

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

type upload struct {
	action ID
	output ID
	path   string
	size   int64
}

// magic prefixes every shared object. It carries the format version so a
// future change of layout cannot be misread as a corrupt object.
const magic = "gocache1"

// maxHeader bounds the header scan, so a hostile or truncated object cannot
// make the reader allocate without limit.
const maxHeader = 256

func NewRemote(s Store, prefix string, write bool, getDeadline, putDeadline time.Duration, maxSize int64, workers int, limit int64) *Remote {
	r := &Remote{
		store:       s,
		prefix:      prefix,
		write:       write,
		getDeadline: getDeadline,
		putDeadline: putDeadline,
		maxSize:     maxSize,
		limit:       limit,
		queue:       make(chan upload, 256),
		done:        make(chan struct{}),
	}
	var wg sync.WaitGroup
	for range workers {
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

func (r *Remote) key(a ID) string {
	s := a.String()
	b := s
	if len(s) >= 2 {
		b = s[:2]
	}
	return r.prefix + "/" + b + "/" + s
}

func (r *Remote) ok() bool { return !r.tripped.Load() }

// Disable turns the shared tier off for the rest of the process. Setup that
// cannot complete -- no credentials, an unparseable address -- reports here
// once rather than failing every action in turn.
func (r *Remote) Disable(err error) {
	if r.tripped.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "gocache: shared tier off: %v\n", err)
	}
}

// pass records a healthy answer. A miss is a healthy answer: an empty shared
// cache must not look like a broken one.
func (r *Remote) pass() { r.fails.Store(0) }

func (r *Remote) trip(err error) {
	r.Fails.Add(1)
	if r.fails.Add(1) >= r.limit && r.tripped.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, "gocache: shared tier disabled after %d failures: %v\n", r.limit, err)
	}
}

// Get fetches an action result from the shared tier. Every failure mode,
// including a corrupt or forged object, returns a miss: the build then
// compiles the action locally, which is always correct.
func (r *Remote) Get(a ID) (ID, []byte, bool) {
	if !r.ok() {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.getDeadline)
	defer cancel()

	body, err := r.store.Get(ctx, r.key(a))
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

	// An object that does not match its own digest counts against the failure
	// budget, the same as a transport error. That is deliberate: a store
	// handing out bytes that fail their integrity check is not healthy, and
	// tripping early bounds how much of it a build looks at. The build is
	// correct either way, because both paths compile locally.
	out, data, err := readObject(body, r.maxSize)
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
	if !r.write || !r.ok() || size > r.maxSize {
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
	head := header(u.output, u.size)
	sum, n, err := hashFile(head, u.path)
	if err != nil || n != u.size {
		// The file is gone or no longer the size the record claims. Nothing to
		// upload, and nothing broken about the shared tier.
		r.Dropped.Add(1)
		return
	}
	f, err := os.Open(u.path)
	if err != nil {
		r.Dropped.Add(1)
		return
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), r.putDeadline)
	defer cancel()

	total := int64(len(head)) + n
	err = r.store.Put(ctx, r.key(u.action), total, io.MultiReader(bytes.NewReader(head), f), sum)
	if err != nil {
		r.trip(err)
		return
	}
	r.pass()
	r.Uploads.Add(1)
	r.UpBytes.Add(total)
}

// Drain stops accepting uploads and waits for the queued ones, up to a
// deadline. It is called after the go command has already been released, so
// the wait delays nothing but this program's own exit.
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

// header is the one line that precedes the body in a shared object:
// magic, output digest, byte count.
func header(o ID, size int64) []byte {
	return fmt.Appendf(nil, "%s %s %d\n", magic, o, size)
}

// readObject parses a shared object and verifies it. The digest check is the
// integrity boundary of this program: bytes arriving from shared storage are
// only handed to the compiler once sha256(body) matches the output ID the
// object claims, so neither corruption at rest nor a tampered body can reach a
// build intact. It does not authenticate the producer -- that is what
// write-scoped credentials are for.
func readObject(rc io.Reader, max int64) (ID, []byte, error) {
	br := bufio.NewReader(io.LimitReader(rc, max+maxHeader))
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, nil, fmt.Errorf("header: %w", err)
	}
	if len(line) > maxHeader {
		return nil, nil, errors.New("header too long")
	}
	var kind, digest string
	var size int64
	if _, err := fmt.Sscan(line, &kind, &digest, &size); err != nil {
		return nil, nil, fmt.Errorf("header: %w", err)
	}
	if kind != magic {
		return nil, nil, fmt.Errorf("bad magic %q", kind)
	}
	if size < 0 || size > max {
		return nil, nil, fmt.Errorf("size %d out of range", size)
	}
	out, err := parseID(digest)
	if err != nil {
		return nil, nil, fmt.Errorf("output id: %w", err)
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, nil, fmt.Errorf("body: %w", err)
	}
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], out) {
		return nil, nil, errors.New("body does not match its output id")
	}
	return out, body, nil
}

// parseSize accepts a plain byte count or one with a K/M/G suffix.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

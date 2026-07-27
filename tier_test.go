package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mem is a shared tier under test control: it can be empty, it can be slow, it
// can be broken, and it counts every call so a test can prove that a broken
// one stops being called.
type mem struct {
	mu   sync.Mutex
	data map[string][]byte

	err   error         // returned by every operation when set
	delay time.Duration // applied before every operation

	Gets, Puts atomic.Int64
}

func newMem() *mem { return &mem{data: map[string][]byte{}} }

func (m *mem) sleep(ctx context.Context) error {
	if m.delay == 0 {
		return nil
	}
	select {
	case <-time.After(m.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *mem) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.Gets.Add(1)
	if err := m.sleep(ctx); err != nil {
		return nil, err
	}
	if m.err != nil {
		return nil, m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, ErrMiss
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *mem) Put(ctx context.Context, key string, size int64, body io.Reader, hash string) error {
	m.Puts.Add(1)
	if err := m.sleep(ctx); err != nil {
		return err
	}
	if m.err != nil {
		return m.err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size mismatch")
	}
	if sum := sha256.Sum256(b); hash != hexOf(sum[:]) {
		return errors.New("hash mismatch")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
	return nil
}

func hexOf(b []byte) string { return ID(b).String() }

// seed writes an object directly into the shared tier, as another machine's
// build would have left it: correctly sealed for the action it is stored under.
func (m *mem) seed(prefix string, a ID, body []byte) {
	s := a.String()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[prefix+"/"+s[:2]+"/"+s] = honest(a, body)
}

// open is the shared tier as it looks once credentials have arrived.
func open(s Store, p Policy) *Remote {
	r := NewRemote(p)
	r.Open(&session{store: s, sealKey: sealKey}, nil)
	return r
}

func newTier(t *testing.T, s Store, write bool) (*Tier, *Remote) {
	t.Helper()
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return &Tier{disk: d}, nil
	}
	r := open(s, Policy{
		Prefix: "p", Write: write,
		Get: 2 * time.Second, Put: 5 * time.Second,
		MaxSize: 32 << 20, Workers: 2, Limit: 3,
	})
	return &Tier{disk: d, remote: r}, r
}

func id(b byte) ID {
	v := make([]byte, 32)
	for i := range v {
		v[i] = b
	}
	return v
}

func outputOf(body []byte) ID {
	s := sha256.Sum256(body)
	return s[:]
}

// A result written by this build is served from local disk, without the shared
// tier being consulted at all.
func TestLocalHit(t *testing.T) {
	m := newMem()
	tier, r := newTier(t, m, true)
	body := []byte("compiled object")

	if _, err := tier.Put(id(1), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	e, ok := tier.Get(id(1))
	if !ok {
		t.Fatal("want hit")
	}
	if e.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", e.Size, len(body))
	}
	got, err := readFile(e.Path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, %v", got, err)
	}
	if n := m.Gets.Load(); n != 0 {
		t.Errorf("shared tier read %d times on a local hit, want 0", n)
	}
	if tier.Local.Load() != 1 {
		t.Errorf("local hits = %d, want 1", tier.Local.Load())
	}
	r.Drain(time.Second)
}

// A result another machine produced is fetched from the shared tier and
// promoted, so the second lookup costs nothing.
func TestSharedHitPromotesToLocal(t *testing.T) {
	m := newMem()
	tier, _ := newTier(t, m, false)
	body := []byte("object built elsewhere")
	m.seed("p", id(2), body)

	e, ok := tier.Get(id(2))
	if !ok {
		t.Fatal("want shared hit")
	}
	if got, err := readFile(e.Path); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, %v", got, err)
	}
	if tier.Shared.Load() != 1 {
		t.Errorf("shared hits = %d, want 1", tier.Shared.Load())
	}

	before := m.Gets.Load()
	if _, ok := tier.Get(id(2)); !ok {
		t.Fatal("want local hit after promotion")
	}
	if m.Gets.Load() != before {
		t.Error("second lookup crossed the network; promotion did not happen")
	}
	if tier.Local.Load() != 1 {
		t.Errorf("local hits = %d, want 1", tier.Local.Load())
	}
}

// Nothing anywhere is a miss, and a miss is not a failure: it must not count
// against the shared tier's health.
func TestFullMiss(t *testing.T) {
	m := newMem()
	tier, r := newTier(t, m, false)

	if _, ok := tier.Get(id(3)); ok {
		t.Fatal("want miss")
	}
	if tier.Misses.Load() != 1 {
		t.Errorf("misses = %d, want 1", tier.Misses.Load())
	}
	if r.Fails.Load() != 0 {
		t.Errorf("an empty shared tier counted %d failures, want 0", r.Fails.Load())
	}
	if !r.ok() {
		t.Error("an empty shared tier disabled itself")
	}
}

// A new result is written to local disk and pushed to the shared tier, where
// another machine can find it under the same action ID.
func TestWriteThrough(t *testing.T) {
	m := newMem()
	tier, r := newTier(t, m, true)
	body := []byte("freshly compiled")

	if _, err := tier.Put(id(4), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	r.Drain(5 * time.Second)

	if r.Uploads.Load() != 1 {
		t.Fatalf("uploads = %d, want 1", r.Uploads.Load())
	}

	// Prove it by reading it back through a cache with an empty local tier,
	// which is exactly the other machine's position.
	other, _ := newTier(t, m, false)
	e, ok := other.Get(id(4))
	if !ok {
		t.Fatal("another machine could not find the uploaded result")
	}
	if got, err := readFile(e.Path); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, %v", got, err)
	}
}

// Read-only is the default. A cache without write permission must never upload.
func TestReadOnlyDoesNotUpload(t *testing.T) {
	m := newMem()
	tier, r := newTier(t, m, false)
	body := []byte("local only")

	if _, err := tier.Put(id(5), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	r.Drain(time.Second)
	if n := m.Puts.Load(); n != 0 {
		t.Errorf("read-only cache uploaded %d times, want 0", n)
	}
	if _, ok := tier.Get(id(5)); !ok {
		t.Error("read-only cache lost its own local result")
	}
}

// The central requirement. With the shared tier broken, every operation still
// succeeds locally, the failure is noticed once, and the shared tier stops
// being called at all.
func TestSharedUnreachableDegradesToLocal(t *testing.T) {
	m := newMem()
	m.err = errors.New("connection refused")
	tier, r := newTier(t, m, true)

	body := []byte("built while the shared tier was down")
	if _, err := tier.Put(id(6), outputOf(body), body); err != nil {
		t.Fatalf("put failed with a broken shared tier: %v", err)
	}
	e, ok := tier.Get(id(6))
	if !ok {
		t.Fatal("local result unavailable with a broken shared tier")
	}
	if got, err := readFile(e.Path); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("body = %q, %v", got, err)
	}

	// Every unknown action is a plain miss, and after the failure budget the
	// broken tier is not consulted again.
	for i := range 200 {
		if _, ok := tier.Get(id(byte(100 + i%50))); ok {
			t.Fatal("unexpected hit from a broken shared tier")
		}
	}
	if !r.tripped.Load() {
		t.Fatal("broken shared tier was never disabled")
	}
	if n := m.Gets.Load(); n > 3 {
		t.Errorf("broken shared tier was called %d times; the breaker allows 3", n)
	}
	r.Drain(time.Second)
}

// A shared tier that hangs must cost a bounded amount of time in total, not a
// deadline per action. This is what keeps a wedged store from turning a
// three-minute build into an hour.
func TestHangingSharedTierCostsBoundedTime(t *testing.T) {
	m := newMem()
	m.delay = time.Hour // never answers
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const deadline = 200 * time.Millisecond
	r := open(m, Policy{Prefix: "p", Get: deadline, Put: deadline, MaxSize: 32 << 20, Workers: 2, Limit: 3})
	tier := &Tier{disk: d, remote: r}

	start := time.Now()
	for i := range 500 {
		tier.Get(id(byte(i % 200)))
	}
	elapsed := time.Since(start)

	// Three attempts are allowed to time out; everything after is instant.
	if max := 10 * deadline; elapsed > max {
		t.Errorf("500 actions against a hung shared tier took %v, want under %v", elapsed, max)
	}
	if n := m.Gets.Load(); n > 3 {
		t.Errorf("hung shared tier was called %d times, want at most 3", n)
	}
	t.Logf("500 lookups against a hung shared tier: %v (breaker tripped after %d calls)", elapsed, m.Gets.Load())
}

// Uploads never block the build. With a shared tier that stalls, a put returns
// at local-disk speed.
func TestUploadNeverBlocks(t *testing.T) {
	m := newMem()
	m.delay = 30 * time.Second
	tier, r := newTier(t, m, true)

	start := time.Now()
	for i := range 100 {
		body := []byte(strings.Repeat("x", 64) + string(rune(i)))
		if _, err := tier.Put(id(byte(i)), outputOf(body), body); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("100 puts took %v with a stalled shared tier; uploads are blocking the build", elapsed)
	}
	t.Logf("100 puts with a stalled shared tier: %v, %d queued uploads dropped", elapsed, r.Dropped.Load())

	// Every result is still readable locally.
	for i := range 100 {
		if _, ok := tier.Get(id(byte(i))); !ok {
			t.Fatalf("local result %d lost", i)
		}
	}
}

// Bytes from shared storage reach the compiler only if they hash to the output
// ID the object claims. Corruption at rest and tampering in transit both land
// here.
func TestCorruptSharedObjectIsAMiss(t *testing.T) {
	m := newMem()
	tier, _ := newTier(t, m, false)
	body := []byte("honest object")
	m.seed("p", id(7), body)

	// Flip the payload while leaving the claimed digest intact.
	key := "p/" + id(7).String()[:2] + "/" + id(7).String()
	m.mu.Lock()
	obj := m.data[key]
	obj[len(obj)-1] ^= 0xff
	m.mu.Unlock()

	if _, ok := tier.Get(id(7)); ok {
		t.Fatal("a tampered shared object was served to the build")
	}
}

func TestTruncatedSharedObjectIsAMiss(t *testing.T) {
	m := newMem()
	tier, _ := newTier(t, m, false)
	body := bytes.Repeat([]byte("z"), 4096)
	m.seed("p", id(8), body)

	key := "p/" + id(8).String()[:2] + "/" + id(8).String()
	m.mu.Lock()
	m.data[key] = m.data[key][:100]
	m.mu.Unlock()

	if _, ok := tier.Get(id(8)); ok {
		t.Fatal("a truncated shared object was served to the build")
	}
}

func TestGarbageSharedObjectIsAMiss(t *testing.T) {
	for _, junk := range []string{
		"",
		"\n",
		"not-gocache abc 5\nhello",
		"gocache1 nothex 5\nhello",
		"gocache1 " + id(9).String() + " -1\n",
		"gocache1 " + id(9).String() + " 99999999999\nshort",
		strings.Repeat("A", 4096),
	} {
		m := newMem()
		tier, _ := newTier(t, m, false)
		key := "p/" + id(9).String()[:2] + "/" + id(9).String()
		m.data[key] = []byte(junk)
		if _, ok := tier.Get(id(9)); ok {
			t.Errorf("garbage object %q was served to the build", junk)
		}
	}
}

// Objects larger than the configured limit stay local. They must not be
// uploaded and their absence must not look like a failure.
func TestOversizeStaysLocal(t *testing.T) {
	m := newMem()
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := open(m, Policy{Prefix: "p", Write: true, Get: time.Second, Put: time.Second, MaxSize: 1024, Workers: 2, Limit: 3})
	tier := &Tier{disk: d, remote: r}

	body := bytes.Repeat([]byte("b"), 4096)
	if _, err := tier.Put(id(10), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	r.Drain(time.Second)
	if m.Puts.Load() != 0 {
		t.Errorf("oversize object was uploaded")
	}
	if r.tripped.Load() {
		t.Error("skipping an oversize object disabled the shared tier")
	}
	if _, ok := tier.Get(id(10)); !ok {
		t.Error("oversize object lost locally")
	}
}

func TestNoRemoteIsPlainLocalCache(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	body := []byte("no shared tier configured")
	if _, err := tier.Put(id(11), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	if _, ok := tier.Get(id(11)); !ok {
		t.Fatal("want local hit")
	}
	if _, ok := tier.Get(id(12)); ok {
		t.Fatal("want miss")
	}
}

func TestConcurrentGetsAndPuts(t *testing.T) {
	m := newMem()
	tier, r := newTier(t, m, true)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(i)}, 128)
			if _, err := tier.Put(id(byte(i)), outputOf(body), body); err != nil {
				t.Error(err)
				return
			}
			if _, ok := tier.Get(id(byte(i))); !ok {
				t.Errorf("lost %d", i)
			}
		}(i)
	}
	wg.Wait()
	r.Drain(5 * time.Second)
}

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

// Offering an upload while the queue is being drained must not panic. A send
// on a closed channel would kill the process, and a dead cache program is a
// failed build.
func TestOfferDuringDrainIsSafe(t *testing.T) {
	m := newMem()
	tier, r := newTier(t, m, true)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(i)}, 32)
			for range 50 {
				tier.Put(id(byte(i)), outputOf(body), body)
			}
		}(i)
	}
	go r.Drain(2 * time.Second)
	wg.Wait()
	r.Drain(2 * time.Second) // idempotent
}

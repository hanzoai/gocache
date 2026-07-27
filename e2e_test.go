package main

import (
	"bytes"
	"cmp"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests run the real go command against the built binary. They are the
// only ones that prove the thing that matters: that a build finishes.

// Every go command these tests start gets its own GOCACHE, never the machine's.
// A test suite for a build cache must not touch the build cache the machine is
// using: other work on the same box reads that directory, and a suite that
// writes to it can turn someone else's build red -- or, far worse, green against
// artifacts they never built.
//
// The directory is under TMPDIR, so pointing TMPDIR at a tmpfs keeps the whole
// suite off the shared disk. It is left in place between runs on purpose: it
// costs one cold compile to create and nothing after that.
func isolatedCache(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "gocache-test-gocache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func goTool(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	const std = "/usr/local/go/bin/go"
	if _, err := os.Stat(std); err == nil {
		return std
	}
	t.Skip("no go toolchain")
	return ""
}

// build compiles gocache once per test binary and returns its path.
var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

func gocacheBinary(t *testing.T) string {
	t.Helper()
	cache := isolatedCache(t)
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gocache-bin-")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "gocache")
		cmd := exec.Command(goTool(t), "build", "-o", buildPath, ".")
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local",
			"GOCACHE="+cache, "GOCACHEPROG=")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("building gocache: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

// scratch writes a small module with enough packages to exercise the cache.
func scratch(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module scratch\n\ngo 1.24\n")
	for i := range 12 {
		write(fmt.Sprintf("p%d/p%d.go", i, i), fmt.Sprintf(`package p%d

import (
	"fmt"
	"strings"
)

func F%d(s string) string { return fmt.Sprint(strings.ToUpper(s), %d) }
`, i, i, i))
	}
	var imports, calls strings.Builder
	for i := range 12 {
		fmt.Fprintf(&imports, "\t\"scratch/p%d\"\n", i)
		fmt.Fprintf(&calls, "\tfmt.Print(p%d.F%d(\"x\"))\n", i, i)
	}
	write("main.go", fmt.Sprintf("package main\n\nimport (\n\t\"fmt\"\n%s)\n\nfunc main() {\n%s}\n", imports.String(), calls.String()))
	return dir
}

type buildEnv struct {
	cacheDir string
	remote   string
	creds    bool // supply the three secrets the shared tier needs
	write    bool
	verbose  bool
	timeout  string
}

// outcome is one build: what a person waited for, what the machine actually
// spent, and everything written to the build's output.
//
// The two times answer different questions. Wall time is what a developer
// experiences and is worthless on a machine with other tenants. CPU covers the
// go command and every compiler it reaps, so work genuinely avoided shows up
// there no matter how busy the box is.
type outcome struct {
	wall time.Duration
	cpu  time.Duration
	out  string
}

func runBuild(t *testing.T, mod string, e buildEnv) outcome {
	t.Helper()
	bin := gocacheBinary(t)
	// A multi-package build needs a directory to write into.
	out := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(),
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GOFLAGS=",
		// Never the machine's cache. With GOCACHEPROG set the go command uses
		// this only for its fuzz directory, but a test suite for a build cache
		// has no business anywhere near the build cache other work depends on.
		"GOCACHE="+isolatedCache(t),
		"GOCACHEPROG="+bin,
		"GOCACHE_DIR="+e.cacheDir,
		"GOCACHE_REMOTE="+e.remote,
		"GOCACHE_TIMEOUT="+cmp.Or(e.timeout, "2s"),
		"GOCACHE_DRAIN=20s",
		// Keep the test hermetic: no ambient machine identity, so nothing
		// reaches out to a real KMS.
		"KMS_CLIENT_ID=",
		"KMS_CLIENT_SECRET=",
	)
	if e.creds {
		// The three secrets arrive together or not at all, which is the same
		// rule production runs under.
		env = append(env,
			"GOCACHE_KEY=an-access-key",
			"GOCACHE_SECRET=a-secret-key",
			"GOCACHE_SEAL="+string(sealKey),
		)
	}
	if e.write {
		env = append(env, "GOCACHE_WRITE=1")
	}
	if e.verbose {
		env = append(env, "GOCACHE_VERBOSE=1")
	}

	cmd := exec.Command(goTool(t), "build", "-o", out, "./...")
	cmd.Dir = mod
	cmd.Env = env

	// A real file, not an in-memory buffer. Handed a buffer, exec.Cmd makes a
	// pipe and copies from it, and Wait returns only once every writer has
	// closed that pipe -- including this cache program, which outlives the go
	// command deliberately. Timing a build through such a pipe would time the
	// uploads and the trim as well: exactly the work the design moves off the
	// build's critical path. A file has no such rendezvous, so what is measured
	// here is what a developer waits for.
	log, err := os.CreateTemp(t.TempDir(), "build-log-")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log

	start := time.Now()
	err = cmd.Run()
	elapsed := time.Since(start)

	var cpu time.Duration
	if st := cmd.ProcessState; st != nil {
		cpu = st.UserTime() + st.SystemTime()
	}

	if e.verbose {
		// Statistics are the last thing written, after the uploads. Waiting for
		// them happens after the build has already been timed.
		awaitStats(t, log.Name())
	} else {
		// Nothing is expected on this path, so there is no line to wait for. A
		// short settle turns "nothing yet" into "nothing at all", which is what
		// makes the silence test able to fail.
		time.Sleep(500 * time.Millisecond)
	}
	b, _ := os.ReadFile(log.Name())
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, b)
	}
	return outcome{wall: elapsed, cpu: cpu, out: string(b)}
}

// awaitStats waits for the cache program to finish and report. It is the only
// place a test waits on that process, and never before a measurement.
func awaitStats(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), " gets,") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the cache program never reported statistics")
}

// The go command accepts this program as its cache and completes a build with
// nothing shared configured.
func TestBuildWithLocalTierOnly(t *testing.T) {
	mod := scratch(t)
	dir := t.TempDir()
	r := runBuild(t, mod, buildEnv{cacheDir: dir, verbose: true})
	if strings.Contains(r.out, "panic") {
		t.Fatalf("cache program panicked:\n%s", r.out)
	}
	if !strings.Contains(r.out, "gocache: ") {
		t.Errorf("no statistics reported:\n%s", r.out)
	}
	t.Logf("%s", strings.TrimSpace(r.out))
}

// A build populates the shared tier; a second machine, with an empty local
// tier, gets those results back instead of compiling them.
func TestBuildSharesResultsAcrossMachines(t *testing.T) {
	shared := newFakeS3(t)
	defer shared.Close()

	mod := scratch(t)

	// Machine one: cold everywhere, uploads what it compiles.
	one := runBuild(t, mod, buildEnv{
		cacheDir: t.TempDir(), remote: shared.dsn, creds: true,
		write: true, verbose: true,
	})
	t.Logf("machine one: %s", stats(one.out))
	if shared.puts.Load() == 0 {
		t.Fatal("nothing was uploaded to the shared tier")
	}

	// Machine two: empty local tier, same source. Everything it needs is in
	// the shared tier.
	two := runBuild(t, mod, buildEnv{
		cacheDir: t.TempDir(), remote: shared.dsn, creds: true,
		verbose: true,
	})
	t.Logf("machine two: %s", stats(two.out))

	hits := field(two.out, "shared,")
	if hits == 0 {
		t.Fatalf("machine two got nothing from the shared tier:\n%s", two.out)
	}
	t.Logf("machine two served %d actions from the shared tier", hits)
}

// The requirement that outranks every speedup: a broken shared tier must not
// break the build. Three flavours of broken, one after the other.
func TestBuildSurvivesBrokenSharedTier(t *testing.T) {
	// A port nothing is listening on: connection refused, immediately.
	refused := freePort(t)

	// A listener that accepts and never answers: the worst case, because
	// nothing fails fast.
	black := blackhole(t)
	defer black.Close()

	// A server that answers every request with a server error.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer broken.Close()

	for _, c := range []struct{ name, remote string }{
		{"refused", "s3://b/v1?endpoint=http://127.0.0.1:" + refused},
		{"hangs", "s3://b/v1?endpoint=http://" + black.Addr().String()},
		{"errors", "s3://b/v1?endpoint=" + broken.URL},
		{"garbage-dsn", "not-a-url-at-all"},
		{"wrong-scheme", "redis://host/v1"},
		{"no-endpoint", "s3://bucket/v1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mod := scratch(t)
			r := runBuild(t, mod, buildEnv{
				cacheDir: t.TempDir(), remote: c.remote,
				creds: true, write: true, verbose: true,
			})
			if strings.Contains(r.out, "panic") {
				t.Fatalf("cache program panicked:\n%s", r.out)
			}
			t.Logf("built in %v with a %s shared tier; %s", r.wall.Round(time.Millisecond), c.name, stats(r.out))
		})
	}
}

// The most important test here. A shared tier that hangs -- accepts the
// connection and then never answers, which is what a wedged gateway, a
// blackholed route or a saturated store looks like -- must not slow the build
// down. A build tool that can stall every engineer at once is worse than no
// build tool, so this asserts a bound rather than reporting a number.
//
// Three bounds. Two are exact and hold on any machine; the third is wall time,
// which on a shared box is mostly noise and is therefore given a wide budget.
//
//   - The shared tier is consulted a constant number of times, not once per
//     action. The constant is limit+inflight, because every request already past
//     the breaker's check when the first failure lands still has to come back.
//     What matters is that it does not grow with the build.
//   - CPU is unchanged. Waiting on a socket costs no CPU, so a hung shared tier
//     must not add any. This is the assertion that survives a loaded machine.
//   - Wall time stays within the breaker's own bound plus slack.
func TestHangingSharedTierDoesNotSlowTheBuild(t *testing.T) {
	black := blackhole(t)
	defer black.Close()

	const deadline = time.Second
	mod := scratch(t)

	// What this build costs with no shared tier at all.
	base := runBuild(t, mod, buildEnv{cacheDir: t.TempDir(), timeout: "1s"})

	// The same build, equally cold, against a tier that never answers.
	hung := runBuild(t, mod, buildEnv{
		cacheDir: t.TempDir(),
		remote:   "s3://b/v1?endpoint=http://" + black.Addr().String(),
		creds:    true, write: true, verbose: true, timeout: "1s",
	})

	if strings.Contains(hung.out, "panic") {
		t.Fatalf("cache program panicked:\n%s", hung.out)
	}
	if !strings.Contains(hung.out, "shared tier off") {
		t.Errorf("a hung shared tier was never disabled:\n%s", hung.out)
	}

	gets, fails := field(hung.out, "gets,"), field(hung.out, "fail,")
	if max := 3 + inflight; fails > max {
		t.Errorf("a hung shared tier was waited on %d times, want at most %d", fails, max)
	}
	if gets > 0 && fails >= gets {
		t.Errorf("every one of %d actions waited on the hung shared tier; the breaker did nothing", gets)
	}

	// Waiting on a dead socket is not work. Anything more than a small margin
	// here would mean the cache is burning CPU on a tier that cannot answer.
	if base.cpu > 0 && hung.cpu > base.cpu*3/2 {
		t.Errorf("a hung shared tier cost %v of CPU against %v without one",
			hung.cpu.Round(time.Millisecond), base.cpu.Round(time.Millisecond))
	}

	budget := 3*deadline + 20*time.Second
	if over := hung.wall - base.wall - budget; over > 0 {
		t.Errorf("build took %v against a hung shared tier and %v without one: %v over the %v budget\n%s",
			hung.wall.Round(time.Millisecond), base.wall.Round(time.Millisecond),
			over.Round(time.Millisecond), budget, hung.out)
	}
	t.Logf("wall: %v local-only, %v hung. cpu: %v local-only, %v hung. %d of %d actions reached the tier. %s",
		base.wall.Round(time.Millisecond), hung.wall.Round(time.Millisecond),
		base.cpu.Round(time.Millisecond), hung.cpu.Round(time.Millisecond),
		fails, gets, stats(hung.out))
}

// Silence is a feature. Absent GOCACHE_VERBOSE this program must write nothing
// at all, even when the shared tier is misconfigured or dead: the go command's
// stderr belongs to the build, and a cache that cannot help has nothing to say.
// A line of explanation on every build is how a tool teaches people to distrust
// it.
func TestNothingIsPrintedWithoutVerbose(t *testing.T) {
	black := blackhole(t)
	defer black.Close()

	mod := scratch(t)
	for name, remote := range map[string]string{
		"no shared tier":  "",
		"hangs":           "s3://b/v1?endpoint=http://" + black.Addr().String(),
		"garbage address": "not-a-url-at-all",
	} {
		t.Run(name, func(t *testing.T) {
			// Read-only, so there is nothing to drain and the cache program
			// exits with the build rather than outliving it.
			r := runBuild(t, mod, buildEnv{
				cacheDir: t.TempDir(), remote: remote,
				creds: true, timeout: "1s",
			})
			if strings.Contains(r.out, "gocache") {
				t.Errorf("wrote to the build's output without being asked:\n%s", r.out)
			}
		})
	}
}

// A real build leaves a bounded cache behind: the trim ran, so the directory has
// a ceiling rather than growing for as long as the machine keeps building.
func TestARealBuildTrimsItsCache(t *testing.T) {
	mod := scratch(t)
	dir := t.TempDir()
	runBuild(t, mod, buildEnv{cacheDir: dir, verbose: true})

	if _, err := os.Stat(filepath.Join(dir, "trim")); err != nil {
		t.Errorf("no trim stamp after a build: %v", err)
	}
}

// With no credentials at all the build must still work. This is the state of
// every machine that has not been set up yet.
func TestBuildWithoutCredentials(t *testing.T) {
	shared := newFakeS3(t)
	defer shared.Close()
	mod := scratch(t)

	r := runBuild(t, mod, buildEnv{
		cacheDir: t.TempDir(), remote: shared.dsn, verbose: true,
		// no key, no secret, and no KMS identity in the environment
	})
	if strings.Contains(r.out, "panic") {
		t.Fatalf("cache program panicked:\n%s", r.out)
	}
	if !strings.Contains(r.out, "shared tier off") {
		t.Errorf("missing credentials were not reported:\n%s", r.out)
	}
	t.Logf("%s", stats(r.out))
}

// A shared tier that serves corrupt objects must not corrupt the build.
func TestBuildIgnoresCorruptSharedObjects(t *testing.T) {
	shared := newFakeS3(t)
	shared.corrupt.Store(true)
	defer shared.Close()

	mod := scratch(t)
	// Populate first with a healthy tier.
	shared.corrupt.Store(false)
	runBuild(t, mod, buildEnv{cacheDir: t.TempDir(), remote: shared.dsn, creds: true, write: true})

	// Now corrupt everything on the way out and build from an empty local tier.
	shared.corrupt.Store(true)
	r := runBuild(t, mod, buildEnv{cacheDir: t.TempDir(), remote: shared.dsn, creds: true, verbose: true})
	if strings.Contains(r.out, "panic") {
		t.Fatalf("cache program panicked:\n%s", r.out)
	}
	if got := field(r.out, "shared,"); got != 0 {
		t.Errorf("%d corrupt objects were accepted, want 0", got)
	}
	t.Logf("corrupt shared tier: %s", stats(r.out))
}

// fakeS3 is an S3-compatible object store in memory. It checks that every
// request carries a SigV4 Authorization header, which is what proves the
// signer works end to end.
type fakeS3 struct {
	*httptest.Server
	dsn string

	mu   sync.Mutex
	data map[string][]byte

	gets, puts atomic.Int64
	corrupt    atomic.Bool
	unsigned   atomic.Int64
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	f := &fakeS3{data: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") ||
			!strings.Contains(auth, "Signature=") ||
			r.Header.Get("X-Amz-Content-Sha256") == "" ||
			r.Header.Get("X-Amz-Date") == "" {
			f.unsigned.Add(1)
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.gets.Add(1)
			f.mu.Lock()
			b, ok := f.data[r.URL.Path]
			f.mu.Unlock()
			if !ok {
				http.Error(w, "no such key", http.StatusNotFound)
				return
			}
			if f.corrupt.Load() && len(b) > 0 {
				b = bytes.Clone(b)
				b[len(b)-1] ^= 0xff
			}
			w.Write(b)
		case http.MethodPut:
			f.puts.Add(1)
			var buf bytes.Buffer
			buf.ReadFrom(r.Body)
			f.mu.Lock()
			f.data[r.URL.Path] = buf.Bytes()
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "no", http.StatusMethodNotAllowed)
		}
	}))
	f.dsn = "s3://cache/v1?endpoint=" + f.Server.URL + "&region=us-east-1"
	return f
}

// blackhole accepts connections and never writes a byte.
func blackhole(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	return l
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	l.Close()
	return port
}

// stats returns the cache's own summary lines from a build's output.
func stats(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "gocache: ") {
			keep = append(keep, strings.TrimPrefix(line, "gocache: "))
		}
	}
	return strings.Join(keep, " | ")
}

// field reads the number preceding a label in the statistics line.
func field(out, label string) int {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i, w := range f {
			if w == label && i > 0 {
				var n int
				fmt.Sscan(f[i-1], &n)
				return n
			}
		}
	}
	return 0
}

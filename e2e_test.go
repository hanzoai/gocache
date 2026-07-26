package main

import (
	"bytes"
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
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gocache-bin-")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "gocache")
		cmd := exec.Command(goTool(t), "build", "-o", buildPath, ".")
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
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
	key      string
	secret   string
	write    bool
	verbose  bool
}

func runBuild(t *testing.T, mod string, e buildEnv) (time.Duration, string) {
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
		"GOCACHEPROG="+bin,
		"GOCACHE_DIR="+e.cacheDir,
		"GOCACHE_REMOTE="+e.remote,
		"GOCACHE_KEY="+e.key,
		"GOCACHE_SECRET="+e.secret,
		"GOCACHE_TIMEOUT=2s",
		"GOCACHE_DRAIN=20s",
		// Keep the test hermetic: no ambient machine identity, so nothing
		// reaches out to a real KMS.
		"KMS_CLIENT_ID=",
		"KMS_CLIENT_SECRET=",
	)
	if e.write {
		env = append(env, "GOCACHE_WRITE=1")
	}
	if e.verbose {
		env = append(env, "GOCACHE_VERBOSE=1")
	}

	cmd := exec.Command(goTool(t), "build", "-o", out, "./...")
	cmd.Dir = mod
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("go build failed: %v\n%s", err, buf.String())
	}
	return elapsed, buf.String()
}

// The go command accepts this program as its cache and completes a build with
// nothing shared configured.
func TestBuildWithLocalTierOnly(t *testing.T) {
	mod := scratch(t)
	dir := t.TempDir()
	_, out := runBuild(t, mod, buildEnv{cacheDir: dir, verbose: true})
	if strings.Contains(out, "panic") {
		t.Fatalf("cache program panicked:\n%s", out)
	}
	if !strings.Contains(out, "gocache: ") {
		t.Errorf("no statistics reported:\n%s", out)
	}
	t.Logf("%s", strings.TrimSpace(out))
}

// A build populates the shared tier; a second machine, with an empty local
// tier, gets those results back instead of compiling them.
func TestBuildSharesResultsAcrossMachines(t *testing.T) {
	shared := newFakeS3(t)
	defer shared.Close()

	mod := scratch(t)

	// Machine one: cold everywhere, uploads what it compiles.
	_, out1 := runBuild(t, mod, buildEnv{
		cacheDir: t.TempDir(), remote: shared.dsn, key: "k", secret: "s",
		write: true, verbose: true,
	})
	t.Logf("machine one: %s", stats(out1))
	if shared.puts.Load() == 0 {
		t.Fatal("nothing was uploaded to the shared tier")
	}

	// Machine two: empty local tier, same source. Everything it needs is in
	// the shared tier.
	_, out2 := runBuild(t, mod, buildEnv{
		cacheDir: t.TempDir(), remote: shared.dsn, key: "k", secret: "s",
		verbose: true,
	})
	t.Logf("machine two: %s", stats(out2))

	hits := field(out2, "shared,")
	if hits == 0 {
		t.Fatalf("machine two got nothing from the shared tier:\n%s", out2)
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
			elapsed, out := runBuild(t, mod, buildEnv{
				cacheDir: t.TempDir(), remote: c.remote,
				key: "k", secret: "s", write: true, verbose: true,
			})
			if strings.Contains(out, "panic") {
				t.Fatalf("cache program panicked:\n%s", out)
			}
			t.Logf("built in %v with a %s shared tier; %s", elapsed.Round(time.Millisecond), c.name, stats(out))
		})
	}
}

// With no credentials at all the build must still work. This is the state of
// every machine that has not been set up yet.
func TestBuildWithoutCredentials(t *testing.T) {
	shared := newFakeS3(t)
	defer shared.Close()
	mod := scratch(t)

	_, out := runBuild(t, mod, buildEnv{
		cacheDir: t.TempDir(), remote: shared.dsn, verbose: true,
		// no key, no secret, and no KMS identity in the environment
	})
	if strings.Contains(out, "panic") {
		t.Fatalf("cache program panicked:\n%s", out)
	}
	if !strings.Contains(out, "shared tier off") {
		t.Errorf("missing credentials were not reported:\n%s", out)
	}
	t.Logf("%s", stats(out))
}

// A shared tier that serves corrupt objects must not corrupt the build.
func TestBuildIgnoresCorruptSharedObjects(t *testing.T) {
	shared := newFakeS3(t)
	shared.corrupt.Store(true)
	defer shared.Close()

	mod := scratch(t)
	// Populate first with a healthy tier.
	shared.corrupt.Store(false)
	runBuild(t, mod, buildEnv{cacheDir: t.TempDir(), remote: shared.dsn, key: "k", secret: "s", write: true})

	// Now corrupt everything on the way out and build from an empty local tier.
	shared.corrupt.Store(true)
	_, out := runBuild(t, mod, buildEnv{cacheDir: t.TempDir(), remote: shared.dsn, key: "k", secret: "s", verbose: true})
	if strings.Contains(out, "panic") {
		t.Fatalf("cache program panicked:\n%s", out)
	}
	if got := field(out, "shared,"); got != 0 {
		t.Errorf("%d corrupt objects were accepted, want 0", got)
	}
	t.Logf("corrupt shared tier: %s", stats(out))
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
				b = append(append([]byte{}, b...))
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

// Command gocache is a build cache for the go command with a shared second
// tier.
//
// The go command speaks the GOCACHEPROG protocol to a program named by the
// GOCACHEPROG environment variable, which lets the build cache live somewhere
// other than local disk. gocache keeps the local disk tier and adds a shared
// one: on a local miss it looks in object storage, and it writes new results
// back. A machine that has never built the tree gets the compiler output that
// another machine already produced.
//
// It caches compiles. Linking is one process that loads every symbol in the
// program and is not cached by the go command at all, so a build whose time
// goes into linking will not move.
//
// Configuration is environment only, because the three places this runs -- a
// laptop, a CI job, a PaaS builder -- all set environment variables and none of
// them share a config file.
//
//	GOCACHE_DIR       local tier directory        (default ~/.cache/gocache)
//	GOCACHE_REMOTE    s3://bucket/prefix?endpoint=https://host&region=r
//	GOCACHE_WRITE     1 to upload results         (default read-only)
//	GOCACHE_KEY       access key, delivered from KMS by the environment
//	GOCACHE_SECRET    secret key, delivered from KMS by the environment
//	GOCACHE_SEAL      seal key, delivered from KMS by the environment
//	GOCACHE_KMS       KMS secret path to read all three from directly
//	GOCACHE_TIMEOUT   deadline per shared operation (default 3s)
//	GOCACHE_MAX       largest object to share      (default 32M)
//	GOCACHE_DRAIN     how long to finish uploads after the build (default 30s)
//	GOCACHE_VERBOSE   1 to print cache statistics to stderr
//	KMS_ADDR KMS_ORG KMS_ENV KMS_CLIENT_ID KMS_CLIENT_SECRET
//
// Nothing is written to stderr unless GOCACHE_VERBOSE is set. A shared tier that
// is unreachable is meant to cost a developer nothing, and a line of
// explanation on every build is not nothing.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// verbose gates everything this program writes to stderr. The go command's
// stderr belongs to the build. A cache that cannot help must not editorialize:
// a shared tier that is unreachable is supposed to cost a developer nothing,
// and a line of explanation on every build for a week is not nothing. What
// happened is available on request, in one statistics line, including why the
// shared tier is off.
var verbose bool

func logf(format string, args ...any) {
	if verbose {
		fmt.Fprintf(os.Stderr, "gocache: "+format+"\n", args...)
	}
}

func main() {
	verbose = envBool("GOCACHE_VERBOSE")

	// A panic here is a failed build in every repository that has the cache
	// turned on. Nothing reaches the go command but the protocol.
	//
	// This one message is printed whether or not statistics were asked for.
	// Reaching it means this program has a bug, the go command is about to
	// abort the build over a cache program that vanished, and the developer
	// deserves to know which one.
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "gocache: %v\n", p)
			os.Exit(0)
		}
	}()

	disk, err := OpenDisk(cacheDir())
	if err != nil {
		// Without a local tier there is nothing useful to do, and pretending
		// otherwise would make every action a miss forever. Exit before the
		// handshake so the go command reports a clear startup failure.
		fmt.Fprintf(os.Stderr, "gocache: %v\n", err)
		os.Exit(1)
	}
	tier := &Tier{disk: disk}

	var remote *Remote
	if dsn := os.Getenv("GOCACHE_REMOTE"); dsn != "" {
		// The address is parsed now, because it is pure string work and its
		// prefix is read on every request. Only the credentials, which cost
		// network round trips, are resolved later.
		cfg, err := parseRemote(dsn)
		if err != nil {
			logf("%v; local only", err)
		} else {
			deadline := envDuration("GOCACHE_TIMEOUT", 3*time.Second)
			remote = NewRemote(Policy{
				Prefix:  cfg.prefix,
				Write:   envBool("GOCACHE_WRITE"),
				Get:     deadline,
				Put:     deadline * 10,
				MaxSize: envSize("GOCACHE_MAX", 32<<20),
				Workers: 4,
				Limit:   3,
			})
			tier.remote = remote

			// Resolving credentials happens behind the handshake, never in
			// front of it: the go command waits for the capability line with
			// no timeout of its own.
			go remote.Open(cfg.open())
		}
	}

	Serve(os.Stdin, os.Stdout, tier)

	// Closing stdout is what releases the go command: it waits for this
	// program's stdout to reach EOF and then exits. Everything after this line
	// costs the build nothing.
	os.Stdout.Close()

	if remote != nil {
		remote.Drain(envDuration("GOCACHE_DRAIN", 30*time.Second))
	}
	// Trimming last, after the uploads that read these same files. Both cost
	// the build nothing: the go command left when stdout closed.
	disk.Trim(time.Now())
	if verbose {
		report(os.Stderr, tier, remote)
	}
}

// remoteConfig is everything the DSN says: where the shared tier is and which
// keyspace of it this cache owns.
type remoteConfig struct {
	endpoint *url.URL
	bucket   string
	prefix   string
	region   string
}

// parseRemote reads s3://bucket/prefix?endpoint=https://host&region=r.
func parseRemote(dsn string) (remoteConfig, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return remoteConfig{}, fmt.Errorf("GOCACHE_REMOTE: %w", err)
	}
	if u.Scheme != "s3" {
		return remoteConfig{}, fmt.Errorf("GOCACHE_REMOTE: unsupported scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return remoteConfig{}, errors.New("GOCACHE_REMOTE: no bucket")
	}
	q := u.Query()
	endpoint := q.Get("endpoint")
	if endpoint == "" {
		return remoteConfig{}, errors.New("GOCACHE_REMOTE: no endpoint")
	}
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return remoteConfig{}, fmt.Errorf("GOCACHE_REMOTE: bad endpoint %q", endpoint)
	}

	cfg := remoteConfig{
		endpoint: base,
		bucket:   u.Host,
		prefix:   strings.Trim(u.Path, "/"),
		region:   q.Get("region"),
	}
	if cfg.prefix == "" {
		cfg.prefix = "v1"
	}
	if cfg.region == "" {
		cfg.region = "us-east-1"
	}
	return cfg, nil
}

// open resolves everything the shared tier needs before its first request. All
// three secrets travel together and are required together: a cache that can
// reach the store but cannot authenticate its objects would have to either
// accept unsigned ones or reject every one, and both are worse than being off.
func (c remoteConfig) open() (*session, error) {
	k, err := secrets()
	if err != nil {
		return nil, err
	}
	if len(k.seal) < minSeal {
		return nil, fmt.Errorf("seal key is %d bytes, need at least %d", len(k.seal), minSeal)
	}
	return &session{
		store: &S3{
			HTTP:     &http.Client{Transport: transport()},
			Endpoint: c.endpoint,
			Bucket:   c.bucket,
			Region:   c.region,
			Key:      k.access,
			Secret:   k.secret,
		},
		sealKey: k.seal,
	}, nil
}

// minSeal is the shortest seal key worth having. Anything shorter is a
// placeholder somebody meant to replace, and the whole point of the key is that
// guessing it is not an option.
const minSeal = 16

// keys is what the shared tier is unlocked with: two for the store, one for the
// objects in it.
type keys struct {
	access string
	secret string
	seal   []byte
}

// secrets never come from a file in a repository or a literal in this program.
// Either the environment already carries them, because a KMS-synced secret put
// them there, or this asks KMS directly with a machine identity.
func secrets() (keys, error) {
	a, s, k := os.Getenv("GOCACHE_KEY"), os.Getenv("GOCACHE_SECRET"), os.Getenv("GOCACHE_SEAL")
	if a != "" && s != "" && k != "" {
		return keys{a, s, []byte(k)}, nil
	}
	path := env("GOCACHE_KMS", "gocache")
	id, secret := os.Getenv("KMS_CLIENT_ID"), os.Getenv("KMS_CLIENT_SECRET")
	if id == "" || secret == "" {
		return keys{}, errors.New("no credentials: set GOCACHE_KEY, GOCACHE_SECRET and GOCACHE_SEAL, or KMS_CLIENT_ID and KMS_CLIENT_SECRET")
	}
	kms := kms{
		addr:   strings.TrimRight(env("KMS_ADDR", "https://kms.hanzo.ai"), "/"),
		org:    env("KMS_ORG", "hanzo"),
		env:    env("KMS_ENV", "prod"),
		id:     id,
		secret: secret,
		http:   &http.Client{Transport: transport(), Timeout: 15 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	v, err := kms.read(ctx, path, "access-key", "secret-key", "seal-key")
	if err != nil {
		return keys{}, err
	}
	return keys{v[0], v[1], []byte(v[2])}, nil
}

// transport keeps connections warm across the thousands of actions in a build
// and refuses to wait forever on any stage of one.
func transport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
}

func report(w io.Writer, t *Tier, r *Remote) {
	local, shared, miss := t.Local.Load(), t.Shared.Load(), t.Misses.Load()
	total := local + shared + miss
	fmt.Fprintf(w, "gocache: %d gets, %d local, %d shared, %d miss\n", total, local, shared, miss)
	if r == nil {
		return
	}
	fmt.Fprintf(w, "gocache: shared %d hit, %d miss, %d fail, %d up, %d dropped, %dK down, %dK up\n",
		r.Hits.Load(), r.Misses.Load(), r.Fails.Load(), r.Uploads.Load(), r.Dropped.Load(),
		r.DownBytes.Load()>>10, r.UpBytes.Load()>>10)
	// The reason, not just the fact. A shared tier that quietly stopped helping
	// is the failure mode this program is most likely to have in production,
	// and one line here is the difference between noticing and not.
	if why := r.Why(); why != "" {
		fmt.Fprintf(w, "gocache: shared tier off: %s\n", why)
	}
}

func cacheDir() string {
	if d := os.Getenv("GOCACHE_DIR"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "gocache")
	}
	return filepath.Join(os.TempDir(), "gocache")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string) bool {
	switch os.Getenv(k) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func envSize(k string, def int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := parseSize(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
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

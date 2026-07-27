package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ID is a cache key or a content hash. The go command uses 32-byte SHA-256
// digests for both action IDs (the key) and output IDs (the content hash).
type ID []byte

func (id ID) String() string { return hex.EncodeToString(id) }

func parseID(s string) (ID, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("empty id")
	}
	return ID(b), nil
}

// Entry is one cached action result: which output it produced, how big that
// output is, when it was recorded, and the local file holding the bytes.
type Entry struct {
	Output ID
	Size   int64
	Time   time.Time
	Path   string
}

// Disk is the local tier. It is content-addressed in two namespaces: an action
// record naming an output, and the output bytes themselves. Two actions that
// produce identical bytes share one output file.
type Disk struct{ root string }

func OpenDisk(root string) (*Disk, error) {
	if root == "" {
		return nil, errors.New("empty cache directory")
	}
	// The protocol requires DiskPath to be absolute, and the go command opens
	// those paths from whatever directory it was started in.
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := refuseGoCache(abs); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Disk{root: abs}, nil
}

// refuseGoCache rejects the go command's own cache directory as the local tier.
//
// The two caches have different layouts and different retention. Sharing one
// directory would put this program's entries where the go command's trim will
// never remove them, and this program's scan among files it does not recognise.
// A build cache that can damage a developer's build cache is worse than no build
// cache, so this is refused at startup, before the handshake, where the go
// command reports it plainly instead of building against a directory two
// programs are rearranging.
func refuseGoCache(dir string) error {
	same := func(other string) bool {
		if other == "" {
			return false
		}
		abs, err := filepath.Abs(other)
		return err == nil && abs == dir
	}
	if same(os.Getenv("GOCACHE")) {
		return fmt.Errorf("GOCACHE_DIR is the go command's own cache (%s); give this cache its own directory", dir)
	}
	// The documented default, for the common case where GOCACHE is not exported.
	if d, err := os.UserCacheDir(); err == nil && same(filepath.Join(d, "go-build")) {
		return fmt.Errorf("GOCACHE_DIR is the go command's default cache (%s); give this cache its own directory", dir)
	}
	return nil
}

// shard keeps directories small: 256 buckets by the first byte of the digest.
func (d *Disk) shard(kind string, id ID) string {
	s := id.String()
	b := s
	if len(s) >= 2 {
		b = s[:2]
	}
	return filepath.Join(d.root, kind, b, s)
}

func (d *Disk) actionPath(a ID) string { return d.shard("a", a) }

// OutputPath is where the bytes for an output ID live. The go command holds
// this path for the rest of its run, so the file must not be removed while a
// build is in flight.
func (d *Disk) OutputPath(o ID) string { return d.shard("o", o) }

// Get returns the entry for an action, or false. A record whose output file is
// missing or the wrong size is treated as absent, not as an error: a torn or
// trimmed cache must degrade to a miss.
//
// A hit marks both files as used, which is what keeps a live entry out of a
// scan's way. The modification times come from reads Get already had to make,
// so recording use costs no extra system call.
func (d *Disk) Get(a ID) (Entry, bool) {
	action := d.actionPath(a)
	rec, used, err := readRecord(action)
	if err != nil {
		return Entry{}, false
	}
	e, err := parseRecord(string(rec))
	if err != nil {
		return Entry{}, false
	}
	e.Path = d.OutputPath(e.Output)
	fi, err := os.Stat(e.Path)
	if err != nil || fi.Size() != e.Size {
		return Entry{}, false
	}
	now := time.Now()
	d.mark(action, used, now)
	d.mark(e.Path, fi.ModTime(), now)
	return e, true
}

// Put stores the output bytes and the action record. The output is written
// first so no record can ever name bytes that are not on disk yet.
func (d *Disk) Put(a, o ID, body []byte) (Entry, error) {
	now := time.Now()
	e := Entry{Output: o, Size: int64(len(body)), Time: now, Path: d.OutputPath(o)}
	if fi, err := os.Stat(e.Path); err != nil || fi.Size() != e.Size {
		if err := writeFile(e.Path, body); err != nil {
			return Entry{}, err
		}
	} else {
		// The bytes are already here, from this build or an older one. Mark
		// them, because an output being written to right now is as live as one
		// being read.
		d.mark(e.Path, fi.ModTime(), now)
	}
	if err := writeFile(d.actionPath(a), []byte(formatRecord(e))); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// maxRecord bounds a read of the action namespace. A record is one short line;
// anything longer is not one, and a Get that met such a file would reject it
// anyway. Reading it in full first would be a way to make this program
// allocate.
const maxRecord = 256

// readRecord returns a record's bytes and its modification time. os.ReadFile
// would discard the time and cost a second stat to recover it.
func readRecord(path string) ([]byte, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	if fi.Size() > maxRecord {
		return nil, time.Time{}, fmt.Errorf("record is %d bytes", fi.Size())
	}
	b := make([]byte, fi.Size())
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, time.Time{}, err
	}
	return b, fi.ModTime(), nil
}

// A record is one line: output digest, byte count, and unix nanoseconds.
func formatRecord(e Entry) string {
	return fmt.Sprintf("%s %d %d\n", e.Output, e.Size, e.Time.UnixNano())
}

func parseRecord(s string) (Entry, error) {
	f := strings.Fields(s)
	if len(f) != 3 {
		return Entry{}, fmt.Errorf("record has %d fields, want 3", len(f))
	}
	out, err := parseID(f[0])
	if err != nil {
		return Entry{}, err
	}
	size, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return Entry{}, err
	}
	if size < 0 {
		return Entry{}, errors.New("negative size")
	}
	ns, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Output: out, Size: size, Time: time.Unix(0, ns)}, nil
}

// writeFile publishes data at path by renaming a sibling temporary file, so a
// concurrent reader sees either the old contents or the new ones, never a
// partial write. Several go processes share one cache directory.
func writeFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

package main

import (
	"encoding/hex"
	"errors"
	"fmt"
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
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Disk{root: root}, nil
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
func (d *Disk) Get(a ID) (Entry, bool) {
	rec, err := os.ReadFile(d.actionPath(a))
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
	return e, true
}

// Put stores the output bytes and the action record. The output is written
// first so no record can ever name bytes that are not on disk yet.
func (d *Disk) Put(a, o ID, body []byte) (Entry, error) {
	e := Entry{Output: o, Size: int64(len(body)), Time: time.Now(), Path: d.OutputPath(o)}
	if fi, err := os.Stat(e.Path); err != nil || fi.Size() != e.Size {
		if err := writeFile(e.Path, body); err != nil {
			return Entry{}, err
		}
	}
	if err := writeFile(d.actionPath(a), []byte(formatRecord(e))); err != nil {
		return Entry{}, err
	}
	return e, nil
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

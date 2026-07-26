package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDiskRoundTrip(t *testing.T) {
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("object bytes")
	e, err := d.Put(id(1), outputOf(body), body)
	if err != nil {
		t.Fatal(err)
	}
	if e.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", e.Size, len(body))
	}
	got, ok := d.Get(id(1))
	if !ok {
		t.Fatal("want hit")
	}
	if got.Path != e.Path {
		t.Errorf("path = %q, want %q", got.Path, e.Path)
	}
	if b, err := os.ReadFile(got.Path); err != nil || !bytes.Equal(b, body) {
		t.Fatalf("contents = %q, %v", b, err)
	}
	if got.Time.IsZero() {
		t.Error("no timestamp recorded")
	}
}

// Two actions with the same output share one file on disk.
func TestDiskSharesIdenticalOutputs(t *testing.T) {
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("identical")
	a, _ := d.Put(id(1), outputOf(body), body)
	b, _ := d.Put(id(2), outputOf(body), body)
	if a.Path != b.Path {
		t.Errorf("identical outputs stored twice: %q and %q", a.Path, b.Path)
	}
}

// A record whose output file has been trimmed away is a miss, not an error and
// not a path the go command would fail to open.
func TestDiskMissingOutputIsAMiss(t *testing.T) {
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("will be trimmed")
	e, err := d.Put(id(3), outputOf(body), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.Path); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Get(id(3)); ok {
		t.Error("a record pointing at a deleted file reported a hit")
	}
}

// A truncated output file is a miss: the go command must never be handed a
// short object.
func TestDiskShortOutputIsAMiss(t *testing.T) {
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("x"), 1000)
	e, _ := d.Put(id(4), outputOf(body), body)
	if err := os.WriteFile(e.Path, body[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.Get(id(4)); ok {
		t.Error("a truncated output reported a hit")
	}
}

func TestDiskCorruptRecordIsAMiss(t *testing.T) {
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"", "\n", "garbage", "zz 1 2", "abcd -5 1", "abcd 1"} {
		if err := writeFile(d.actionPath(id(5)), []byte(junk)); err != nil {
			t.Fatal(err)
		}
		if _, ok := d.Get(id(5)); ok {
			t.Errorf("corrupt record %q reported a hit", junk)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	in := Entry{Output: id(9), Size: 12345, Time: time.Unix(0, 1700000000123456789)}
	out, err := parseRecord(formatRecord(in))
	if err != nil {
		t.Fatal(err)
	}
	if out.Output.String() != in.Output.String() || out.Size != in.Size || !out.Time.Equal(in.Time) {
		t.Errorf("round trip changed the record: %+v -> %+v", in, out)
	}
}

// Several go processes share one cache directory. Writes are published by
// rename, so a reader never sees a half-written file.
func TestDiskConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	body := bytes.Repeat([]byte("shared"), 4096)
	out := outputOf(body)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := OpenDisk(root)
			if err != nil {
				t.Error(err)
				return
			}
			for i := range 20 {
				if _, err := d.Put(id(byte(i)), out, body); err != nil {
					t.Error(err)
					return
				}
				if e, ok := d.Get(id(byte(i))); ok {
					b, err := os.ReadFile(e.Path)
					if err != nil || !bytes.Equal(b, body) {
						t.Errorf("torn read: %d bytes, %v", len(b), err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	// No temporary files should be left behind.
	var leftovers int
	filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && len(e.Name()) > 5 && e.Name()[:5] == ".tmp-" {
			leftovers++
		}
		return nil
	})
	if leftovers != 0 {
		t.Errorf("%d temporary files left behind", leftovers)
	}
}

func TestOpenDiskRejectsEmptyRoot(t *testing.T) {
	if _, err := OpenDisk(""); err == nil {
		t.Error("empty cache directory was accepted")
	}
}

func TestParseRemote(t *testing.T) {
	c, err := parseRemote("s3://bucket/some/prefix?endpoint=https://s3.hanzo.ai&region=us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.bucket != "bucket" || c.prefix != "some/prefix" || c.region != "us-east-1" ||
		c.endpoint.String() != "https://s3.hanzo.ai" {
		t.Errorf("parsed %+v", c)
	}

	c, err = parseRemote("s3://bucket?endpoint=http://s3.hanzo.svc:9000")
	if err != nil {
		t.Fatal(err)
	}
	if c.prefix != "v1" || c.region != "us-east-1" {
		t.Errorf("defaults wrong: %+v", c)
	}

	for _, bad := range []string{
		"", "bucket", "redis://b?endpoint=https://h", "s3:///p?endpoint=https://h",
		"s3://b/p", "s3://b/p?endpoint=", "s3://b/p?endpoint=ftp://h", "s3://b/p?endpoint=notaurl",
	} {
		if _, err := parseRemote(bad); err == nil {
			t.Errorf("parseRemote(%q) was accepted", bad)
		}
	}
}

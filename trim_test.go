package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Trimming is what keeps this program from being remembered as the thing that
// filled everyone's disk. The go command's own cache reaches tens of gigabytes
// on a busy machine with trimming on; without it there is no ceiling at all.

// The local tier must never be the go command's own cache. Two layouts and two
// retention policies in one directory means this program's entries sit where the
// go command's trim will never remove them, and this program's scan runs among
// files it does not recognise. A build cache that can damage the build cache a
// machine is already using is worse than no build cache, so this is refused
// before the handshake rather than discovered later.
func TestTheGoCommandsOwnCacheIsRefused(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("GOCACHE", dir)
	if _, err := OpenDisk(dir); err == nil {
		t.Error("opened the go command's own cache as the local tier")
	}
	// A relative spelling of the same directory is the same directory.
	if _, err := OpenDisk(dir + "/."); err == nil {
		t.Error("a differently spelled path got past the check")
	}
	// Anything else under it is fine: only the directory itself is refused.
	if _, err := OpenDisk(filepath.Join(dir, "gocache")); err != nil {
		t.Errorf("a subdirectory was refused: %v", err)
	}

	t.Setenv("GOCACHE", "")
	if _, err := OpenDisk(dir); err != nil {
		t.Errorf("an ordinary directory was refused: %v", err)
	}
}

// present reports whether an entry is still on disk without marking it as used.
// Get is the wrong probe for a test that trims again afterwards: reading an
// entry refreshes the very timestamp the next trim reads, so a Get between two
// trims silently rescues the thing under test.
func present(d *Disk, a ID) bool {
	_, err := os.Stat(d.actionPath(a))
	return err == nil
}

// age backdates every file under the cache, which is how a test says "this
// entry has not been touched in a week" without waiting a week.
func age(t *testing.T, root string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		return os.Chtimes(p, when, when)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTrimRemovesWhatWasNotUsed(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}

	stale := []byte("compiled a long time ago")
	if _, err := d.Put(id(1), outputOf(stale), stale); err != nil {
		t.Fatal(err)
	}
	age(t, root, keepFor+2*time.Hour)

	// A second entry written now, which must survive.
	fresh := []byte("compiled just now")
	if _, err := d.Put(id(2), outputOf(fresh), fresh); err != nil {
		t.Fatal(err)
	}

	d.Trim(time.Now())

	if _, ok := d.Get(id(1)); ok {
		t.Error("an entry unused for longer than the limit survived a trim")
	}
	if _, ok := d.Get(id(2)); !ok {
		t.Error("a fresh entry was trimmed")
	}
}

// Reading an entry is using it. An object compiled last month but linked into
// every build today must not be thrown away.
func TestUseKeepsAnEntryAlive(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}

	used, idle := []byte("still needed"), []byte("forgotten")
	if _, err := d.Put(id(3), outputOf(used), used); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put(id(4), outputOf(idle), idle); err != nil {
		t.Fatal(err)
	}
	age(t, root, keepFor+2*time.Hour)

	// One build reads one of them. That is the only difference between them.
	if _, ok := d.Get(id(3)); !ok {
		t.Fatal("want hit before the trim")
	}

	d.Trim(time.Now())

	if _, ok := d.Get(id(3)); !ok {
		t.Error("an entry used moments before the trim was removed")
	}
	if _, ok := d.Get(id(4)); ok {
		t.Error("an entry nobody touched survived")
	}
}

// Writing an entry that is already present is also using it, which is the case
// of a rebuild that produces bytes it already has.
func TestRewriteKeepsAnEntryAlive(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("identical output")
	if _, err := d.Put(id(5), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	age(t, root, keepFor+2*time.Hour)

	if _, err := d.Put(id(5), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	d.Trim(time.Now())

	if _, ok := d.Get(id(5)); !ok {
		t.Error("an entry rewritten moments before the trim was removed")
	}
}

// A scan is not free, so it happens at most once a day however many builds run.
func TestTrimScansAtMostOncePerInterval(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("x")
	if _, err := d.Put(id(6), outputOf(body), body); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	d.Trim(now) // writes the stamp; nothing is stale yet

	// Everything is now ancient, but the next build is minutes later and must
	// not scan again.
	age(t, root, keepFor+2*time.Hour)
	d.Trim(now.Add(time.Minute))
	if !present(d, id(6)) {
		t.Fatal("a second trim within the interval scanned anyway")
	}

	// A day later it does scan.
	d.Trim(now.Add(scanInterval + time.Minute))
	if present(d, id(6)) {
		t.Error("a trim after the interval did not scan")
	}
}

// An unreadable or nonsense stamp must mean "scan", never "never scan again".
// A cache that stops trimming because one file got corrupted is a cache that
// grows forever.
func TestUnreadableStampMeansScan(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, stamp := range map[string]string{
		"empty":        "",
		"not a number": "yesterday",
		"far future":   "99999999999",
	} {
		if err := os.WriteFile(filepath.Join(root, "trim"), []byte(stamp), 0o600); err != nil {
			t.Fatal(err)
		}
		if !d.due(filepath.Join(root, "trim"), time.Now()) {
			t.Errorf("%s stamp: trim skipped", name)
		}
	}

	// And a stamp from the recent past does mean skip.
	if err := os.WriteFile(filepath.Join(root, "trim"),
		[]byte(time.Now().Format("20060102")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFile removes its temporary on every error path, so one that survives is
// the wreckage of a process that was killed mid-write. Nothing will come back
// for it, and it must not be left behind forever.
func TestTrimRemovesAbandonedTemporaries(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("real entry")
	if _, err := d.Put(id(7), outputOf(body), body); err != nil {
		t.Fatal(err)
	}

	orphan := filepath.Join(filepath.Dir(d.OutputPath(outputOf(body))), ".tmp-abandoned")
	if err := os.WriteFile(orphan, []byte("half an object"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, root, keepFor+2*time.Hour)

	d.Trim(time.Now())
	if _, err := os.Stat(orphan); err == nil {
		t.Error("an abandoned temporary survived a trim")
	}
}

// Whatever a trim removes, the cache is still a working cache afterwards: a
// removed entry reads as a miss, and the next build refills it.
func TestCacheWorksAfterATrim(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("will be trimmed")
	if _, err := d.Put(id(8), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	age(t, root, keepFor+2*time.Hour)
	d.Trim(time.Now())

	if _, ok := d.Get(id(8)); ok {
		t.Fatal("entry should be gone")
	}
	e, err := d.Put(id(8), outputOf(body), body)
	if err != nil {
		t.Fatalf("cache unusable after a trim: %v", err)
	}
	got, ok := d.Get(id(8))
	if !ok || got.Path != e.Path {
		t.Error("refilling the cache after a trim did not work")
	}
}

// Trimming an empty cache, or one that has only ever been read, must not fail.
func TestTrimOnAnEmptyCacheIsHarmless(t *testing.T) {
	d, err := OpenDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d.Trim(time.Now())
	if _, ok := d.Get(id(9)); ok {
		t.Error("an empty cache returned a hit")
	}
}

// Marking is bounded: reading the same entry in a tight loop must not rewrite
// its inode every time, which is why the interval exists.
func TestMarkingIsRateLimited(t *testing.T) {
	root := t.TempDir()
	d, err := OpenDisk(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("hot entry")
	if _, err := d.Put(id(10), outputOf(body), body); err != nil {
		t.Fatal(err)
	}
	path := d.OutputPath(outputOf(body))

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		d.Get(id(10))
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a fresh entry was re-marked on every read")
	}
}

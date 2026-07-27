package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The go command trims its own cache. A program that replaces it inherits that
// job: without trimming, the local tier grows for as long as the machine keeps
// building, and the first thing a shared build cache would be known for is
// filling developers' disks. The go command's own cache on a busy machine
// reaches tens of gigabytes even with trimming on.
//
// The policy is the go command's, because it was chosen from measurements of
// real Go development (golang.org/issue/22990) and because two caches on one
// machine with different retention would be one more thing to reason about:
//
//   - An entry's modification time is its time of last use, refreshed at most
//     once an hour so that reading the cache does not rewrite every inode.
//   - A scan happens at most once a day.
//   - A scan removes what has not been used in five days.
//
// The Cache interface the go command documents requires that trimming in one
// process never delete an entry another process is still using, and suggests
// keeping anything used in the last day. Five days clears that by a wide
// margin. Trimming also runs after this program has released the go command,
// so it is never on a build's critical path.
const (
	markInterval = time.Hour
	scanInterval = 24 * time.Hour
	keepFor      = 5 * 24 * time.Hour
)

// mark records that an entry was used, given the modification time a caller
// already had in hand. Both tiers of a hit are marked -- the action record and
// the output it names -- so that a scan cannot take the bytes out from under a
// record that is still live.
//
// A failure is not reported. The consequence of a mark that does not happen is
// that a live entry looks idle and may be trimmed early, which costs one
// recompile.
func (d *Disk) mark(path string, mtime, now time.Time) {
	if now.Sub(mtime) < markInterval {
		return
	}
	os.Chtimes(path, now, now)
}

// Trim removes entries that have not been used in keepFor, at most once per
// scanInterval across every process sharing the directory.
//
// The stamp is written before the scan rather than after, so that concurrent
// builds do not all decide to scan at once. Two that race anyway are harmless:
// removing a file twice is idempotent, and every removal is a cache miss at
// worst.
func (d *Disk) Trim(now time.Time) {
	stamp := filepath.Join(d.root, "trim")
	if !d.due(stamp, now) {
		return
	}
	// A stamp that cannot be written means the scan would repeat on every
	// build. Skip it rather than rescan forever.
	if err := writeFile(stamp, []byte(strconv.FormatInt(now.Unix(), 10))); err != nil {
		return
	}

	// The extra markInterval accounts for the mark being up to an hour behind
	// the true time of last use.
	cutoff := now.Add(-keepFor - markInterval)
	for _, kind := range []string{"a", "o"} {
		buckets, err := os.ReadDir(filepath.Join(d.root, kind))
		if err != nil {
			continue
		}
		for _, b := range buckets {
			if b.IsDir() {
				d.sweep(filepath.Join(d.root, kind, b.Name()), cutoff)
			}
		}
	}
}

// due reports whether enough time has passed since the last scan. An
// unreadable or unparseable stamp means scan: an empty scan is cheap, and a
// cache that never trims is not.
//
// A stamp in the future is tolerated up to markInterval, which is the clock
// skew a shared directory can legitimately show; beyond that it is treated as
// wrong and a scan runs.
func (d *Disk) due(stamp string, now time.Time) bool {
	b, err := os.ReadFile(stamp)
	if err != nil {
		return true
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return true
	}
	since := now.Sub(time.Unix(sec, 0))
	return since >= scanInterval || since < -markInterval
}

// sweep removes the stale entries of one bucket. Directory names are read in
// full before anything is removed, because removing during a scan can skip
// entries.
//
// Temporary files are swept on age alone. writeFile removes its own on every
// error path, so one that outlives the cutoff is the remains of a process that
// was killed mid-write, and nothing will ever come back for it.
func (d *Disk) sweep(bucket string, cutoff time.Time) {
	entries, err := os.ReadDir(bucket)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		os.Remove(filepath.Join(bucket, e.Name()))
	}
}

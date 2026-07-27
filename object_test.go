package main

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shared object format is the only place bytes from another machine become
// bytes the compiler links. Everything here is a way of getting something past
// it, and every one of them has to fail.

var sealKey = []byte("a seal key of entirely adequate length")

// honest builds the object a well-behaved writer would leave for action a.
func honest(a ID, body []byte) []byte {
	o := sha256.Sum256(body)
	head := object(o[:], int64(len(body)), seal(sealKey, a, o[:], int64(len(body)), body))
	return append(head, body...)
}

func TestHonestObjectRoundTrips(t *testing.T) {
	body := []byte("compiled output")
	out, got, err := read(bytes.NewReader(honest(id(1), body)), sealKey, id(1), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want %q", got, body)
	}
	if want := sha256.Sum256(body); !bytes.Equal(out, want[:]) {
		t.Error("output id does not match the body")
	}
}

// The attack the digest alone cannot stop: whoever writes the object picks both
// the body and the digest, so a self-consistent forgery is trivial to make. It
// must fail on the MAC.
func TestForgedObjectIsRejected(t *testing.T) {
	evil := []byte("code that was never compiled from this source")
	o := sha256.Sum256(evil)
	// Perfectly self-consistent: correct magic, correct digest, correct size.
	// Only the key is missing.
	forged := append(object(o[:], int64(len(evil)), strings.Repeat("00", 32)), evil...)

	if _, _, err := read(bytes.NewReader(forged), sealKey, id(2), 1<<20); err == nil {
		t.Fatal("a forged object with a valid digest was accepted")
	}
}

// The MAC must be computed under the right key, not merely present.
func TestObjectUnderAnotherKeyIsRejected(t *testing.T) {
	body := []byte("built by someone else's cache")
	other := []byte("a different object key entirely")
	o := sha256.Sum256(body)
	obj := append(object(o[:], int64(len(body)), seal(other, id(3), o[:], int64(len(body)), body)), body...)

	if _, _, err := read(bytes.NewReader(obj), sealKey, id(3), 1<<20); err == nil {
		t.Fatal("an object sealed under a foreign key was accepted")
	}
}

// Substitution: a genuine object, correctly sealed, copied to a different
// action's key. Read access plus write access would be enough to do this, and
// the result would be a real compiler output handed back for the wrong
// compilation. The action ID inside the MAC is what stops it.
func TestObjectMovedToAnotherActionIsRejected(t *testing.T) {
	body := []byte("genuine output of action four")
	obj := honest(id(4), body)

	if _, _, err := read(bytes.NewReader(obj), sealKey, id(4), 1<<20); err != nil {
		t.Fatalf("the object is not valid where it belongs: %v", err)
	}
	if _, _, err := read(bytes.NewReader(obj), sealKey, id(5), 1<<20); err == nil {
		t.Fatal("an object was accepted under an action it was not sealed for")
	}
}

// Corruption and truncation, which is the same requirement from the other side:
// a damaged object must read as absent, never as valid.
func TestDamagedObjectIsRejected(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 4096)
	good := honest(id(6), body)

	flip := func(b []byte, i int) []byte {
		c := bytes.Clone(b)
		c[i] ^= 0xff
		return c
	}
	for name, obj := range map[string][]byte{
		"last byte flipped":  flip(good, len(good)-1),
		"first byte flipped": flip(good, 0),
		"body byte flipped":  flip(good, len(good)-2048),
		"truncated body":     good[:len(good)-1],
		"truncated to head":  good[:80],
		"empty":              {},
		"newline only":       []byte("\n"),
		"header only":        object(sha256.New().Sum(nil), 0, strings.Repeat("00", 32)),
	} {
		if _, _, err := read(bytes.NewReader(obj), sealKey, id(6), 1<<20); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Malformed headers, including the ones that would make this program allocate
// or read without limit if they were believed.
func TestMalformedHeaderIsRejected(t *testing.T) {
	o := sha256.Sum256([]byte("x"))
	m := strings.Repeat("00", 32)
	for name, obj := range map[string]string{
		"wrong magic":       "gocache1 " + ID(o[:]).String() + " 1 " + m + "\nx",
		"no magic":          ID(o[:]).String() + " 1 " + m + "\nx",
		"digest not hex":    magic + " nothex 1 " + m + "\nx",
		"negative size":     magic + " " + ID(o[:]).String() + " -1 " + m + "\n",
		"size beyond limit": magic + " " + ID(o[:]).String() + " 1099511627776 " + m + "\nx",
		"missing mac":       magic + " " + ID(o[:]).String() + " 1\nx",
		"no newline":        magic + " " + ID(o[:]).String() + " 1 " + m,
		"long header":       magic + " " + strings.Repeat("a", 4096) + " 1 " + m + "\nx",
		"junk":              strings.Repeat("A", 4096),
	} {
		if _, _, err := read(strings.NewReader(obj), sealKey, id(7), 1<<20); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An object claiming to be enormous must be refused on the claim, before the
// body is read: believing it is how a cache program gets killed for memory.
func TestOversizeClaimIsRejectedWithoutReading(t *testing.T) {
	body := []byte("small")
	o := sha256.Sum256(body)
	obj := append(object(o[:], 1<<40, strings.Repeat("00", 32)), body...)
	if _, _, err := read(bytes.NewReader(obj), sealKey, id(8), 1<<20); err == nil {
		t.Error("an object claiming a terabyte was accepted")
	}
}

// sealFile is the upload path's view of the same MAC. It must agree with seal,
// or an object this cache writes is one it will not read back.
func TestSealFileAgreesWithSeal(t *testing.T) {
	body := bytes.Repeat([]byte("payload"), 1000)
	path := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	o := sha256.Sum256(body)

	want := seal(sealKey, id(9), o[:], int64(len(body)), body)
	got, err := sealFile(sealKey, id(9), o[:], int64(len(body)), path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("sealFile = %s, seal = %s", got, want)
	}

	// A file that is not the size the record claims must not be sealed at all:
	// the alternative is uploading an object whose header lies about its body.
	if _, err := sealFile(sealKey, id(9), o[:], int64(len(body))+1, path); err == nil {
		t.Error("a body of the wrong size was sealed")
	}
}

// The empty body is a real case: a compile action can produce no bytes, and the
// format has to carry it.
func TestEmptyBodyRoundTrips(t *testing.T) {
	out, got, err := read(bytes.NewReader(honest(id(10), nil)), sealKey, id(10), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("body = %q, want empty", got)
	}
	if want := sha256.Sum256(nil); !bytes.Equal(out, want[:]) {
		t.Error("output id is not the digest of the empty body")
	}
}

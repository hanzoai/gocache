package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// A shared object is one line of header followed by the compiler output:
//
//	gocache2 <outputID> <size> <mac>\n<body>
//
// Two checks stand between shared storage and the compiler, and a build is
// correct whichever one rejects: anything unreadable, unverified or unauthentic
// is a miss, and a miss is compiled locally.
//
// The digest check -- sha256(body) equals the output ID the object claims --
// catches corruption at rest and truncation in transit. It is not an integrity
// boundary on its own, because every field it compares came from the same
// object: whoever wrote the object chose both the body and the digest.
//
// The authenticity check is the boundary. mac is HMAC-SHA256 over the format
// version, the action ID, the output ID, the size and the body, under a key
// held only by machines that participate in this cache. That closes two attacks
// the digest cannot:
//
//  1. Forgery. A build cache is a supply-chain surface: an object accepted for
//     an action ID becomes linked into binaries on every machine that asks for
//     that action. The S3 gateway this runs against has a single identity with
//     Admin on every bucket, shared by every service on the fleet, so "can
//     write to the bucket" is a much weaker statement than "is a build
//     machine". Bucket write access alone now buys nothing.
//
//  2. Substitution. The action ID is inside the MAC, not merely the key the
//     object was found under. Without that, an authentic object for one action
//     could be copied to another action's key and would verify -- handing the
//     compiler a genuine object for the wrong compilation.
//
// The MAC proves an object came from something holding the key. It does not say
// which machine, and it does not survive the key leaking. Write-scoped
// credentials and a key only build machines hold are what keep it meaningful.
const magic = "gocache2"

// maxHeader bounds the header scan, so a truncated or hostile object cannot
// make the reader allocate without limit.
const maxHeader = 256

// object renders the header for a body whose MAC has already been computed.
func object(o ID, size int64, mac string) []byte {
	return fmt.Appendf(nil, "%s %s %d %s\n", magic, o, size, mac)
}

// seal computes the MAC binding an action to the output it produced.
//
// Every field is fixed-width or length-prefixed by the delimiter, so no two
// distinct inputs render the same message: the version and both IDs are
// fixed-length hex, and the size is decimal and delimited before the body
// begins.
func seal(key []byte, a, o ID, size int64, body []byte) string {
	h := hmac.New(sha256.New, key)
	fmt.Fprintf(h, "%s %s %s %d\n", magic, a, o, size)
	h.Write(body)
	return ID(h.Sum(nil)).String()
}

// sealFile is seal over a body still on disk, for the upload path, which must
// not hold a whole object in memory per worker.
func sealFile(key []byte, a, o ID, size int64, path string) (string, error) {
	h := hmac.New(sha256.New, key)
	fmt.Fprintf(h, "%s %s %s %d\n", magic, a, o, size)
	n, err := copyFile(h, path)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("body is %d bytes, record says %d", n, size)
	}
	return ID(h.Sum(nil)).String(), nil
}

// read parses and verifies one shared object stored under action a. It returns
// the output ID and body only if the object is well formed, matches its own
// digest, and carries a MAC this key produces.
func read(rc io.Reader, key []byte, a ID, max int64) (ID, []byte, error) {
	br := bufio.NewReader(io.LimitReader(rc, max+maxHeader))
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, nil, fmt.Errorf("header: %w", err)
	}
	if len(line) > maxHeader {
		return nil, nil, errors.New("header too long")
	}
	var kind, digest, mac string
	var size int64
	if _, err := fmt.Sscan(line, &kind, &digest, &size, &mac); err != nil {
		return nil, nil, fmt.Errorf("header: %w", err)
	}
	if kind != magic {
		return nil, nil, fmt.Errorf("bad magic %q", kind)
	}
	if size < 0 || size > max {
		return nil, nil, fmt.Errorf("size %d out of range", size)
	}
	out, err := parseID(digest)
	if err != nil {
		return nil, nil, fmt.Errorf("output id: %w", err)
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, nil, fmt.Errorf("body: %w", err)
	}
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], out) {
		return nil, nil, errors.New("body does not match its output id")
	}
	// Constant time, because this comparison is the boundary and a timing
	// oracle on it would let an attacker search for a valid MAC.
	if !hmac.Equal([]byte(seal(key, a, out, size, body)), []byte(mac)) {
		return nil, nil, errors.New("body is not authentic")
	}
	return out, body, nil
}

// copyFile streams a file into w and reports how many bytes it read.
func copyFile(w io.Writer, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(w, f)
}

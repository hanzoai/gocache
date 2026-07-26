package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Vectors published by AWS in "Examples: Signature Version 4 signing".
// Both use exactly the header set this signer produces
// (host;x-amz-content-sha256;x-amz-date), so the expected signatures apply
// unmodified. A 64-hex-digit match is not something that happens by accident:
// these pin the canonical request, the scope, the key derivation and the final
// HMAC all at once.
const (
	vecKey    = "AKIAIOSFODNN7EXAMPLE"
	vecSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	vecHost   = "examplebucket.s3.amazonaws.com"
)

func vecTime(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, "2013-05-24T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func signatureOf(t *testing.T, h http.Header) string {
	t.Helper()
	auth := h.Get("Authorization")
	_, sig, ok := strings.Cut(auth, "Signature=")
	if !ok {
		t.Fatalf("no Signature in %q", auth)
	}
	return sig
}

func TestSignListObjects(t *testing.T) {
	req, err := http.NewRequest("GET", "https://"+vecHost+"/?max-keys=2&prefix=J", nil)
	if err != nil {
		t.Fatal(err)
	}
	sign(req, vecKey, vecSecret, "", "us-east-1", "s3", emptyHash, vecTime(t))

	const want = "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"
	if got := signatureOf(t, req.Header); got != want {
		t.Errorf("signature\n got %s\nwant %s", got, want)
	}
	if got, want := req.Header.Get("X-Amz-Date"), "20130524T000000Z"; got != want {
		t.Errorf("X-Amz-Date = %q, want %q", got, want)
	}
	if !strings.Contains(req.Header.Get("Authorization"),
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Errorf("unexpected SignedHeaders in %q", req.Header.Get("Authorization"))
	}
}

func TestSignGetLifecycle(t *testing.T) {
	req, err := http.NewRequest("GET", "https://"+vecHost+"/?lifecycle", nil)
	if err != nil {
		t.Fatal(err)
	}
	sign(req, vecKey, vecSecret, "", "us-east-1", "s3", emptyHash, vecTime(t))

	const want = "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"
	if got := signatureOf(t, req.Header); got != want {
		t.Errorf("signature\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalPath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/gocache/v1/ab/abcdef", "/gocache/v1/ab/abcdef"},
		{"/a b", "/a%20b"},
		{"/a%20b", "/a%20b"},
		{"/tilde~ok", "/tilde~ok"},
		{"/plus+sign", "/plus%2Bsign"},
	} {
		if got := canonicalPath(c.in); got != c.want {
			t.Errorf("canonicalPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalQuerySorts(t *testing.T) {
	if got, want := canonicalQuery("prefix=J&max-keys=2"), "max-keys=2&prefix=J"; got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
	if got, want := canonicalQuery("lifecycle"), "lifecycle="; got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
}

func TestSignIncludesSecurityToken(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://"+vecHost+"/k", nil)
	sign(req, vecKey, vecSecret, "tok", "us-east-1", "s3", emptyHash, vecTime(t))
	if req.Header.Get("X-Amz-Security-Token") != "tok" {
		t.Error("security token not set")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Error("security token not signed")
	}
}

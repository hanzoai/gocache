package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, the authentication scheme every S3-compatible
// gateway speaks. It is HMAC-SHA256 over a canonical rendering of the request;
// the primitives are stdlib and the format is specified, so implementing it
// here costs about a hundred lines and keeps this program at zero
// dependencies. A build cache holds credentials and feeds bytes into every
// compile, so its supply chain is worth keeping empty.

const (
	algorithm = "AWS4-HMAC-SHA256"

	// emptyHash is sha256 of the empty string, the payload hash of a body-less
	// request.
	emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// sign adds the x-amz-date, x-amz-content-sha256 and Authorization headers to
// req. payload is the hex sha256 of the request body. Only Host and the
// x-amz-* headers are signed, which is the minimum S3 requires and keeps the
// signature independent of whatever headers net/http adds in transit.
func sign(req *http.Request, ak, sk, token, region, service, payload string, now time.Time) {
	utc := now.UTC()
	stamp := utc.Format("20060102T150405Z")
	day := utc.Format("20060102")

	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	names := []string{"host"}
	values := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			names = append(names, lk)
			values[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	sort.Strings(names)

	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(values[n])
		canonHeaders.WriteByte('\n')
	}
	signed := strings.Join(names, ";")

	canonReq := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonHeaders.String(),
		signed,
		payload,
	}, "\n")

	scope := day + "/" + region + "/" + service + "/aws4_request"
	toSign := strings.Join([]string{
		algorithm,
		stamp,
		scope,
		hex.EncodeToString(hash256([]byte(canonReq))),
	}, "\n")

	key := mac(mac(mac(mac([]byte("AWS4"+sk), day), region), service), "aws4_request")
	sig := hex.EncodeToString(macBytes(key, []byte(toSign)))

	req.Header.Set("Authorization", algorithm+
		" Credential="+ak+"/"+scope+
		", SignedHeaders="+signed+
		", Signature="+sig)
}

// canonicalPath re-escapes an already-escaped path so that exactly the
// RFC 3986 unreserved set survives, with "/" kept as the separator. S3 signs
// the non-normalized path.
func canonicalPath(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escape(unescape(s))
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		k, v, _ := strings.Cut(p, "=")
		out = append(out, escape(unescape(k))+"="+escape(unescape(v)))
	}
	sort.Strings(out)
	return strings.Join(out, "&")
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if h, err := hex.DecodeString(s[i+1 : i+3]); err == nil {
				b.Write(h)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

const upper = "0123456789ABCDEF"

func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upper[c>>4])
			b.WriteByte(upper[c&0xf])
		}
	}
	return b.String()
}

func hash256(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func mac(key []byte, data string) []byte { return macBytes(key, []byte(data)) }

func macBytes(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

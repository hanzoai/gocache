package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// S3 talks to any S3-compatible gateway over plain net/http with SigV4. Hanzo
// runs one at s3.hanzo.ai (in-cluster: s3.hanzo.svc:9000). Path-style
// addressing is used because virtual-host style requires wildcard DNS the
// self-hosted gateway does not have.
type S3 struct {
	HTTP     *http.Client
	Endpoint *url.URL
	Bucket   string
	Region   string
	Key      string
	Secret   string
	Token    string
	Now      func() time.Time
}

func (s *S3) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *S3) url(key string) string {
	u := *s.Endpoint
	u.Path = "/" + s.Bucket + "/" + strings.TrimPrefix(key, "/")
	return u.String()
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(key), nil)
	if err != nil {
		return nil, err
	}
	sign(req, s.Key, s.Secret, s.Token, s.Region, "s3", emptyHash, s.now())

	res, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusOK:
		return res.Body, nil
	case http.StatusNotFound:
		res.Body.Close()
		return nil, ErrMiss
	default:
		return nil, statusError(res)
	}
}

// Put uploads size bytes with the given hex sha256. The payload is signed
// rather than sent as UNSIGNED-PAYLOAD: the in-cluster endpoint is plaintext
// HTTP, where an unsigned payload would leave the body unauthenticated.
func (s *S3) Put(ctx context.Context, key string, size int64, body io.Reader, hash string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.url(key), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	sign(req, s.Key, s.Secret, s.Token, s.Region, "s3", hash, s.now())

	res, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return statusError(res)
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	return nil
}

// statusError closes the body and reports the status with a bounded excerpt.
// S3 gateways answer with XML; a truncated copy is enough to tell an expired
// key from a missing bucket without ever landing a credential in a log line.
func statusError(res *http.Response) error {
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	return fmt.Errorf("s3: %s: %s", res.Status, strings.TrimSpace(string(msg)))
}

// hashFile returns the hex sha256 of prefix followed by the contents of path,
// and how many bytes of the file it read. The caller uses that count rather
// than a remembered size, so a file that has been trimmed underneath us cannot
// produce a request whose Content-Length disagrees with its body.
func hashFile(prefix []byte, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	h.Write(prefix)
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

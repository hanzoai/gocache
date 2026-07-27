package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// conv drives the protocol the way the go command does: JSON requests on one
// pipe, JSON responses on the other, with put bodies following their request
// as a base64 string.
type conv struct {
	t   *testing.T
	in  *bytes.Buffer
	out *bytes.Buffer
	enc *json.Encoder
}

func newConv(t *testing.T) *conv {
	c := &conv{t: t, in: &bytes.Buffer{}, out: &bytes.Buffer{}}
	c.enc = json.NewEncoder(c.in)
	return c
}

func (c *conv) get(id int64, action ID) {
	c.t.Helper()
	if err := c.enc.Encode(request{ID: id, Command: "get", ActionID: action}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *conv) put(id int64, action, output ID, body []byte) {
	c.t.Helper()
	if err := c.enc.Encode(request{ID: id, Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body))}); err != nil {
		c.t.Fatal(err)
	}
	if len(body) > 0 {
		if err := c.enc.Encode(body); err != nil {
			c.t.Fatal(err)
		}
	}
}

func (c *conv) close(id int64) {
	c.t.Helper()
	if err := c.enc.Encode(request{ID: id, Command: "close"}); err != nil {
		c.t.Fatal(err)
	}
}

func (c *conv) run(tier *Tier) map[int64]response {
	c.t.Helper()
	Serve(c.in, c.out, tier)

	byID := map[int64]response{}
	dec := json.NewDecoder(bufio.NewReader(c.out))
	for {
		var r response
		if err := dec.Decode(&r); err == io.EOF {
			break
		} else if err != nil {
			c.t.Fatalf("decoding response stream: %v", err)
		}
		byID[r.ID] = r
	}
	return byID
}

// The first line must declare the commands, and it must appear before any work
// is done, because the go command blocks on it.
func TestHandshakeIsFirstAndComplete(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	c := newConv(t)
	c.close(1)
	res := c.run(tier)

	hello, ok := res[0]
	if !ok {
		t.Fatal("no handshake")
	}
	want := map[string]bool{"get": true, "put": true, "close": true}
	for _, cmd := range hello.KnownCommands {
		delete(want, cmd)
	}
	if len(want) != 0 {
		t.Errorf("handshake missing %v (got %v)", want, hello.KnownCommands)
	}
	if _, ok := res[1]; !ok {
		t.Error("close was not answered")
	}
}

// A put followed by a get of the same action returns the stored output, with a
// disk path the go command can open.
func TestProtocolRoundTrip(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	body := []byte("package object bytes")
	out := outputOf(body)

	c := newConv(t)
	c.put(1, id(20), out, body)
	c.get(2, id(20))
	c.close(3)
	res := c.run(tier)

	if res[1].Err != "" {
		t.Fatalf("put: %s", res[1].Err)
	}
	if res[1].DiskPath == "" {
		t.Fatal("put returned no DiskPath; the go command requires one")
	}
	if res[2].Miss {
		t.Fatal("get after put reported a miss")
	}
	if !bytes.Equal(res[2].OutputID, out) {
		t.Errorf("OutputID = %x, want %x", res[2].OutputID, out)
	}
	if res[2].Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", res[2].Size, len(body))
	}
	if res[2].Time == nil || res[2].Time.IsZero() {
		t.Error("no Time on a hit")
	}
	got, err := readFile(res[2].DiskPath)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("DiskPath contents = %q, %v", got, err)
	}
}

func TestProtocolMiss(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	c := newConv(t)
	c.get(1, id(21))
	c.close(2)
	res := c.run(tier)

	if !res[1].Miss {
		t.Error("want Miss on an unknown action")
	}
	if res[1].Err != "" {
		t.Errorf("a miss must not be an error, got %q", res[1].Err)
	}
	if res[1].DiskPath != "" {
		t.Error("a miss must not carry a DiskPath")
	}
}

// A zero-length output is a real result; the go command stores plenty of them.
func TestProtocolEmptyBody(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	body := []byte{}
	c := newConv(t)
	c.put(1, id(22), outputOf(body), body)
	c.get(2, id(22))
	c.close(3)
	res := c.run(tier)

	if res[2].Miss {
		t.Fatal("empty output was not stored")
	}
	if res[2].Size != 0 {
		t.Errorf("Size = %d, want 0", res[2].Size)
	}
}

// Every request must get exactly one response carrying its own ID. The go
// command aborts the build on an ID it did not send.
func TestEveryRequestAnsweredOnce(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	c := newConv(t)
	const n = 200
	for i := 1; i <= n; i++ {
		body := bytes.Repeat([]byte{byte(i)}, i)
		c.put(int64(i), id(byte(i)), outputOf(body), body)
	}
	for i := 1; i <= n; i++ {
		c.get(int64(n+i), id(byte(i)))
	}
	c.close(int64(2*n + 1))
	res := c.run(tier)

	for i := 1; i <= 2*n+1; i++ {
		if _, ok := res[int64(i)]; !ok {
			t.Fatalf("request %d never answered", i)
		}
	}
	for i := 1; i <= n; i++ {
		if res[int64(n+i)].Miss {
			t.Errorf("get %d missed after put", i)
		}
	}
}

// Truncated or malformed input ends the conversation cleanly. It must not
// panic, because a crash here fails the build.
func TestMalformedInputDoesNotCrash(t *testing.T) {
	for _, in := range []string{
		"",
		"{",
		"not json at all\n",
		`{"ID":1,"Command":"get"}` + "\n" + `{"ID":2,"Command":`,
		`{"ID":1,"Command":"put","BodySize":10}` + "\n", // body promised, never sent
		`{"ID":1,"Command":"nonsense"}` + "\n",
		`{"ID":1,"Command":"get","ActionID":"!!!!"}` + "\n",
	} {
		tier, _ := newTier(t, nil, false)
		out := &bytes.Buffer{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			Serve(strings.NewReader(in), out, tier)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("Serve hung on input %q", in)
		}
		if !strings.Contains(out.String(), "KnownCommands") {
			t.Errorf("no handshake for input %q", in)
		}
	}
}

// A short or empty action ID must be handled, not indexed into blindly.
func TestShortIDsAreSafe(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	c := newConv(t)
	c.get(1, ID{}) // empty action id
	c.get(2, ID{0xab})
	c.put(3, ID{0xcd}, ID{0xef}, []byte("x"))
	c.get(4, ID{0xcd})
	c.close(5)
	res := c.run(tier)

	if _, ok := res[4]; !ok {
		t.Fatal("short-id get unanswered")
	}
	if res[4].Miss {
		t.Error("short-id put/get did not round-trip")
	}
}

// An unknown command is reported, not fatal, and the conversation continues.
func TestUnknownCommandIsReported(t *testing.T) {
	tier, _ := newTier(t, nil, false)
	c := newConv(t)
	if err := c.enc.Encode(request{ID: 1, Command: "get2"}); err != nil {
		t.Fatal(err)
	}
	c.get(2, id(23))
	c.close(3)
	res := c.run(tier)

	if res[1].Err == "" {
		t.Error("unknown command was not reported")
	}
	if _, ok := res[2]; !ok {
		t.Error("conversation did not continue after an unknown command")
	}
}

func TestParseSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"1024", 1024}, {"1k", 1 << 10}, {"32M", 32 << 20}, {"2G", 2 << 30},
	} {
		got, err := parseSize(c.in)
		if err != nil || got != c.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	if _, err := parseSize("nonsense"); err == nil {
		t.Error("parseSize accepted nonsense")
	}
}

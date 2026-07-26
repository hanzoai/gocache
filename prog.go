package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// The GOCACHEPROG protocol. The go command starts this program, reads one line
// of JSON declaring which commands it understands, then streams requests on
// stdin and reads replies from stdout. Replies may be out of order; each
// carries the ID of the request it answers.
//
// The go command is unforgiving here. cmd/go/internal/cache/prog.go calls
// base.Fatalf if this program exits early, writes malformed JSON, or answers
// an ID it was never asked. Any panic in this file is a failed build in every
// repository that has the cache turned on, so nothing below is allowed to
// panic and nothing below is allowed to block indefinitely.

type request struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// wire serializes replies. Requests are handled concurrently so that a fetch
// from the shared tier overlaps with the rest of the build, but two goroutines
// must never interleave bytes on stdout.
type wire struct {
	mu  sync.Mutex
	bw  *bufio.Writer
	enc *json.Encoder
}

func newWire(w io.Writer) *wire {
	bw := bufio.NewWriter(w)
	return &wire{bw: bw, enc: json.NewEncoder(bw)}
}

func (w *wire) send(res *response) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enc.Encode(res)
	w.bw.Flush()
}

// Serve runs the protocol until stdin closes or a close request arrives.
func Serve(in io.Reader, out io.Writer, t *Tier) {
	w := newWire(out)
	// Announce first, before anything that could be slow. The go command waits
	// for this line and has no timeout on it; credentials and network setup
	// happen behind it, never in front of it.
	w.send(&response{KnownCommands: []string{"get", "put", "close"}})

	dec := json.NewDecoder(in)
	var wg sync.WaitGroup
	gate := make(chan struct{}, 32)

	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			break // stdin closed, or the go command died
		}
		var body []byte
		if req.BodySize > 0 {
			// The body follows the request as a base64 JSON string on its own
			// line. It must be consumed even if the request is unusable, or
			// the stream desynchronizes.
			if err := dec.Decode(&body); err != nil {
				break
			}
		}

		switch req.Command {
		case "close":
			wg.Wait()
			w.send(&response{ID: req.ID})
			return

		case "put":
			// Handled in line, so that anything the go command has stored is
			// visible to every request that follows it. This costs nothing:
			// put bodies already arrive one at a time over a single pipe, so
			// writing them out one at a time adds no serialization that the
			// wire did not already impose. The upload to the shared tier is
			// asynchronous either way.
			w.send(handle(t, req, body))

		default:
			// A get may have to cross the network, and a build issues
			// thousands of them. Handling them concurrently is what keeps one
			// round trip from stalling the whole pipeline.
			wg.Add(1)
			gate <- struct{}{}
			go func(req request) {
				defer wg.Done()
				defer func() { <-gate }()
				w.send(handle(t, req, nil))
			}(req)
		}
	}
	wg.Wait()
}

// handle answers one request. It recovers from any panic below it: a crashed
// cache is a slow build, but a crashed cache program is a failed build.
func handle(t *Tier, req request, body []byte) (res *response) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(os.Stderr, "gocache: recovered in %s: %v\n", req.Command, p)
			res = &response{ID: req.ID, Miss: true}
		}
	}()

	switch req.Command {
	case "get":
		if len(req.ActionID) == 0 {
			return &response{ID: req.ID, Miss: true}
		}
		e, ok := t.Get(req.ActionID)
		if !ok {
			return &response{ID: req.ID, Miss: true}
		}
		at := e.Time
		return &response{
			ID:       req.ID,
			OutputID: e.Output,
			Size:     e.Size,
			Time:     &at,
			DiskPath: e.Path,
		}

	case "put":
		if len(req.ActionID) == 0 || len(req.OutputID) == 0 {
			return &response{ID: req.ID, Err: "put without an action or output id"}
		}
		e, err := t.Put(req.ActionID, req.OutputID, body)
		if err != nil {
			// The go command tolerates a failed put; it just rebuilds next
			// time. Reporting the error is more useful than pretending.
			return &response{ID: req.ID, Err: err.Error()}
		}
		return &response{ID: req.ID, DiskPath: e.Path}

	default:
		return &response{ID: req.ID, Err: "unknown command " + req.Command}
	}
}

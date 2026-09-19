package semgate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	bodyStatusNone      = "none"
	bodyStatusComplete  = "complete"
	bodyStatusTruncated = "truncated"
	bodyStatusPartial   = "partial"
	bodyStatusOmitted   = "omitted"
)

type bodyResult struct {
	snapshot []byte        // body bytes sent to the provider
	status   string        // one of the bodyStatus constants
	oversize bool          // the body exceeds maxBodyBytes
	replay   io.ReadCloser // body for the next handler; yields every original byte
}

// peekBody reads at most maxBodyBytes+1 bytes of the body for evaluation
// without consuming it for the next handler.
func peekBody(r *http.Request, cfg *config) bodyResult {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return bodyResult{status: bodyStatusNone, replay: r.Body}
	}
	if !contentTypeAllowed(r.Header.Get("Content-Type"), cfg.bodyContentTypes) {
		return bodyResult{status: bodyStatusOmitted, replay: r.Body}
	}

	limit := cfg.maxBodyBytes
	if r.ContentLength > limit && cfg.oversizeAction != OversizeActionTruncate {
		return bodyResult{status: bodyStatusOmitted, oversize: true, replay: r.Body}
	}

	p := newPeekReader(r.Body, int(limit)+1)
	buf, done, readErr := p.wait(r.Context(), cfg.bodyReadTimeout)
	res := bodyResult{replay: p}
	switch {
	case int64(len(buf)) > limit:
		res.oversize = true
		if cfg.oversizeAction == OversizeActionTruncate {
			res.status, res.snapshot = bodyStatusTruncated, trimIncompleteRune(buf[:limit])
		} else {
			res.status = bodyStatusOmitted
		}
	case done && errors.Is(readErr, io.EOF):
		res.status, res.snapshot = bodyStatusComplete, buf
	default:
		res.status, res.snapshot = bodyStatusPartial, trimIncompleteRune(buf)
	}

	if res.snapshot != nil && !utf8.Valid(res.snapshot) {
		res.status, res.snapshot = bodyStatusOmitted, nil
	}
	return res
}

func contentTypeAllowed(contentType string, patterns []string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	typ, sub, ok := strings.Cut(strings.ToLower(mediaType), "/")
	if !ok {
		return false
	}
	for _, p := range patterns {
		switch {
		case strings.HasPrefix(p, "+"):
			if strings.HasSuffix(sub, p) {
				return true
			}
		case strings.HasSuffix(p, "/*"):
			if typ == strings.TrimSuffix(p, "/*") {
				return true
			}
		case p == typ+"/"+sub:
			return true
		}
	}
	return false
}

// trimIncompleteRune drops a UTF-8 sequence cut at the end of b.
func trimIncompleteRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
}

// peekReader reads up to limit bytes of src in a goroutine and replays them
// to the next handler, which then continues reading src directly. Reading in
// a goroutine lets the middleware stop waiting for a streaming body without
// changing the connection's read deadline.
type peekReader struct {
	src      io.ReadCloser
	limit    int
	finished chan struct{} // closed when the goroutine exits

	mu   sync.Mutex
	cond *sync.Cond // signaled on every append and on goroutine exit
	buf  []byte     // bytes read by the goroutine; grows up to limit
	pos  int        // bytes already returned to the downstream reader
	done bool       // goroutine has exited
	err  error      // io.EOF, a read error, or nil when the goroutine stopped at limit
}

func newPeekReader(src io.ReadCloser, limit int) *peekReader {
	p := &peekReader{src: src, limit: limit, finished: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	go p.fill()
	return p
}

func (p *peekReader) fill() {
	defer close(p.finished)
	chunk := make([]byte, min(p.limit, 32<<10))
	var err error
	// Only this goroutine appends to buf, so reading len(buf) here needs no lock.
	for len(p.buf) < p.limit {
		n, rerr := p.src.Read(chunk[:min(p.limit-len(p.buf), len(chunk))])
		p.mu.Lock()
		p.buf = append(p.buf, chunk[:n]...)
		p.cond.Broadcast()
		p.mu.Unlock()
		if rerr != nil {
			err = rerr
			break
		}
	}
	p.mu.Lock()
	p.done, p.err = true, err
	p.cond.Broadcast()
	p.mu.Unlock()
}

// wait blocks until the goroutine exits, the timeout passes or ctx ends, and
// returns a copy of the bytes read so far.
func (p *peekReader) wait(ctx context.Context, timeout time.Duration) ([]byte, bool, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.finished:
	case <-timer.C:
	case <-ctx.Done():
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return bytes.Clone(p.buf), p.done, p.err
}

func (p *peekReader) Read(b []byte) (int, error) {
	p.mu.Lock()
	for p.pos == len(p.buf) && !p.done {
		p.cond.Wait()
	}
	if p.pos < len(p.buf) {
		n := copy(b, p.buf[p.pos:])
		p.pos += n
		p.mu.Unlock()
		return n, nil
	}
	err := p.err
	p.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return p.src.Read(b)
}

func (p *peekReader) Close() error {
	return p.src.Close()
}

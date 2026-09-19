package semgate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
)

// fakeProvider records requests and answers them with respond.
type fakeProvider struct {
	mu       sync.Mutex
	requests []*providers.Request
	respond  func(ctx context.Context, req *providers.Request) (*providers.Response, error)
}

func (f *fakeProvider) Evaluate(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.respond == nil {
		return &providers.Response{Answers: map[string]json.RawMessage{}}, nil
	}
	return f.respond(ctx, req)
}

func (f *fakeProvider) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeProvider) request(t *testing.T, i int) *providers.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.requests) {
		t.Fatalf("provider received %d requests, want at least %d", len(f.requests), i+1)
	}
	return f.requests[i]
}

// answerByInstructions answers each question with the JSON registered for its
// instructions, so tests do not depend on how question keys are assigned.
func answerByInstructions(answers map[string]string) func(context.Context, *providers.Request) (*providers.Response, error) {
	return func(_ context.Context, req *providers.Request) (*providers.Response, error) {
		out := make(map[string]json.RawMessage, len(req.Questions))
		for key, spec := range req.Questions {
			if a, ok := answers[spec.Instructions]; ok {
				out[key] = json.RawMessage(a)
			}
		}
		return &providers.Response{Answers: out}, nil
	}
}

func noulJSON(p float64) string {
	return fmt.Sprintf(`{"type":"noul","noul":%v}`, p)
}

func choiceJSON(choice string, confidence float64, probs map[string]float64) string {
	b, _ := json.Marshal(map[string]any{"type": "choice", "choice": choice, "confidence": confidence, "probabilities": probs})
	return string(b)
}

func newGate(t *testing.T, p providers.Client, opts ...semgate.Option) *semgate.Gate {
	t.Helper()
	g, err := semgate.New(p, opts...)
	gt.NoError(t, err).Required()
	return g
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected panic, but did not panic")
		}
	}()
	f()
}

func mustNotPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if v := recover(); v != nil {
			t.Errorf("unexpected panic: %v", v)
		}
	}()
	f()
}

// downstream records whether it was called and what it read.
type downstream struct {
	mu      sync.Mutex
	called  bool
	body    []byte
	readErr error
	header  http.Header
}

func (d *downstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	d.mu.Lock()
	d.called, d.body, d.readErr, d.header = true, body, err, r.Header.Clone()
	d.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (d *downstream) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.called, d.body, d.readErr, d.header = false, nil, nil, nil
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func passNoul(w http.ResponseWriter, r *http.Request, _ semgate.NoulAnswer, next http.Handler) {
	next.ServeHTTP(w, r)
}

func passAsk(w http.ResponseWriter, r *http.Request, _ *semgate.Answers, next http.Handler) {
	next.ServeHTTP(w, r)
}

func getRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/", nil)
}

func TestNew(t *testing.T) {
	_, err := semgate.New(&fakeProvider{})
	gt.NoError(t, err)

	_, err = semgate.New(nil)
	gt.Error(t, err)
}

func TestGateMiddlewarePanics(t *testing.T) {
	g := newGate(t, &fakeProvider{})
	choiceFn := func(http.ResponseWriter, *http.Request, semgate.ChoiceAnswer, http.Handler) {}
	scoreFn := func(http.ResponseWriter, *http.Request, semgate.ScoreAnswer, http.Handler) {}

	mustPanic(t, func() { g.Noul("", passNoul) })
	mustPanic(t, func() { g.Noul("q", nil) })
	mustPanic(t, func() { g.Choice("q", options(1), choiceFn) })
	mustPanic(t, func() { g.Choice("q", options(256), choiceFn) })
	mustPanic(t, func() { g.Choice("q", options(2), nil) })
	mustPanic(t, func() { g.Score("q", levels(1), scoreFn) })
	mustPanic(t, func() { g.Score("q", levels(11), scoreFn) })
	mustPanic(t, func() { g.Score("q", levels(2), nil) })
	mustNotPanic(t, func() {
		g.Noul("q", passNoul)
		g.Choice("q", options(2), choiceFn)
		g.Score("q", levels(2), scoreFn)
	})

	q := semgate.Noul("q")
	mustPanic(t, func() { g.Ask(nil, passAsk) })
	mustPanic(t, func() { g.Ask([]semgate.Question{}, passAsk) })
	mustPanic(t, func() { g.Ask([]semgate.Question{nil}, passAsk) })
	mustPanic(t, func() { g.Ask([]semgate.Question{(*semgate.NoulQuestion)(nil)}, passAsk) })
	mustPanic(t, func() { g.Ask([]semgate.Question{q, q}, passAsk) })
	mustPanic(t, func() { g.Ask([]semgate.Question{q}, nil) })
}

func TestGateCallCount(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"a": noulJSON(0), "b": noulJSON(0)})}
	g := newGate(t, p)

	serve(g.Noul("a", passNoul)(&downstream{}), getRequest())
	gt.Number(t, p.calls()).Equal(1)

	stacked := g.Noul("a", passNoul)(g.Noul("b", passNoul)(&downstream{}))
	serve(stacked, getRequest())
	gt.Number(t, p.calls()).Equal(3)
}

func TestGateAskEvaluatesOnce(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{
		"n1": noulJSON(0.1),
		"n2": noulJSON(0.2),
		"c":  choiceJSON("a", 0.9, map[string]float64{"a": 0.9, "b": 0.1}),
		"s":  `{"type":"score","score":0.5,"probabilities":{"0":0.5,"1":0.5},"confidence":0.3}`,
	})}
	mw := newGate(t, p).Ask([]semgate.Question{
		semgate.Noul("n1"),
		semgate.Noul("n2"),
		semgate.Choice("c", map[string]string{"a": "", "b": ""}),
		semgate.Score("s", []string{"low", "high"}),
	}, passAsk)

	d := &downstream{}
	serve(mw(d), getRequest())
	gt.Bool(t, d.called).True()
	gt.Number(t, p.calls()).Equal(1)
	gt.Map(t, p.request(t, 0).Questions).Length(4)
}

func TestGateNext(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": noulJSON(0.9)})}
	g := newGate(t, p)

	t.Run("fn calls next with the whole body", func(t *testing.T) {
		d := &downstream{}
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
		r.Header.Set("Content-Type", "text/plain")
		serve(g.Noul("q", passNoul)(d), r)
		gt.Bool(t, d.called).True()
		gt.String(t, string(d.body)).Equal("hello")
	})

	t.Run("fn responds by itself", func(t *testing.T) {
		deny := g.Noul("q", func(w http.ResponseWriter, _ *http.Request, _ semgate.NoulAnswer, _ http.Handler) {
			http.Error(w, "forbidden", http.StatusForbidden)
		})
		d := &downstream{}
		w := serve(deny(d), getRequest())
		gt.Number(t, w.Code).Equal(http.StatusForbidden)
		gt.Bool(t, d.called).False()
	})

	t.Run("one middleware on two routes", func(t *testing.T) {
		mw := g.Noul("q", passNoul)
		chat, summarize := &downstream{}, &downstream{}
		mux := http.NewServeMux()
		mux.Handle("/chat", mw(chat))
		mux.Handle("/summarize", mw(summarize))

		serve(mux, httptest.NewRequest(http.MethodGet, "/chat", nil))
		gt.Bool(t, chat.called).True()
		gt.Bool(t, summarize.called).False()

		serve(mux, httptest.NewRequest(http.MethodGet, "/summarize", nil))
		gt.Bool(t, summarize.called).True()
	})
}

func TestGateQuestionInTwoAsks(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"shared": noulJSON(0.4), "a": noulJSON(0), "b": noulJSON(0)})}
	g := newGate(t, p)
	shared := semgate.Noul("shared")

	var got []float64
	record := func(_ http.ResponseWriter, _ *http.Request, ans *semgate.Answers, _ http.Handler) {
		got = append(got, shared.Answer(ans).Probability)
	}
	first := g.Ask([]semgate.Question{shared, semgate.Noul("a")}, record)
	second := g.Ask([]semgate.Question{semgate.Noul("b"), shared}, record)

	serve(first(&downstream{}), getRequest())
	serve(second(&downstream{}), getRequest())
	gt.Array(t, got).Equal([]float64{0.4, 0.4})
}

func TestGateConcurrent(t *testing.T) {
	// The provider answers with the request body as the choice, so each
	// request can check it received the answer for its own body.
	p := &fakeProvider{respond: func(_ context.Context, req *providers.Request) (*providers.Response, error) {
		answers := make(map[string]json.RawMessage, len(req.Questions))
		for key := range req.Questions {
			answers[key] = json.RawMessage(choiceJSON(req.State.Body, 1, map[string]float64{req.State.Body: 1}))
		}
		return &providers.Response{Answers: answers}, nil
	}}
	q := semgate.Choice("which?", options(50))
	mw := newGate(t, p).Ask([]semgate.Question{q}, func(_ http.ResponseWriter, r *http.Request, ans *semgate.Answers, _ http.Handler) {
		body, _ := io.ReadAll(r.Body)
		if got := q.Answer(ans).Choice; got != string(body) {
			t.Errorf("answer %q does not match body %q", got, body)
		}
	})
	h := mw(&downstream{})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(fmt.Sprintf("o%d", i)))
			r.Header.Set("Content-Type", "text/plain")
			serve(h, r)
		})
	}
	wg.Wait()
	gt.Number(t, p.calls()).Equal(50)
}

var errProvider = errors.New("provider failed")

func failingProvider() *fakeProvider {
	return &fakeProvider{respond: func(context.Context, *providers.Request) (*providers.Response, error) {
		return nil, errProvider
	}}
}

func TestGateProviderError(t *testing.T) {
	t.Run("single question", func(t *testing.T) {
		called := false
		mw := newGate(t, failingProvider()).Noul("q", func(http.ResponseWriter, *http.Request, semgate.NoulAnswer, http.Handler) {
			called = true
		})
		d := &downstream{}
		w := serve(mw(d), getRequest())
		gt.Number(t, w.Code).Equal(http.StatusServiceUnavailable)
		gt.Bool(t, called).False()
		gt.Bool(t, d.called).False()
	})

	t.Run("ask", func(t *testing.T) {
		called := false
		mw := newGate(t, failingProvider()).Ask([]semgate.Question{semgate.Noul("q")}, func(http.ResponseWriter, *http.Request, *semgate.Answers, http.Handler) {
			called = true
		})
		d := &downstream{}
		w := serve(mw(d), getRequest())
		gt.Number(t, w.Code).Equal(http.StatusServiceUnavailable)
		gt.Bool(t, called).False()
		gt.Bool(t, d.called).False()
	})
}

func TestGateMissingAnswer(t *testing.T) {
	called := false
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"a": noulJSON(0.1)})}
	mw := newGate(t, p).Ask([]semgate.Question{semgate.Noul("a"), semgate.Noul("b")},
		func(http.ResponseWriter, *http.Request, *semgate.Answers, http.Handler) { called = true })
	w := serve(mw(&downstream{}), getRequest())
	gt.Number(t, w.Code).Equal(http.StatusServiceUnavailable)
	gt.Bool(t, called).False()
}

func TestGatePartialFailure(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{
		"a": noulJSON(0.1),
		"b": noulJSON(2),
		"c": noulJSON(0.3),
	})}
	called := false
	mw := newGate(t, p).Ask([]semgate.Question{semgate.Noul("a"), semgate.Noul("b"), semgate.Noul("c")},
		func(http.ResponseWriter, *http.Request, *semgate.Answers, http.Handler) { called = true })
	w := serve(mw(&downstream{}), getRequest())
	gt.Number(t, w.Code).Equal(http.StatusServiceUnavailable)
	gt.Bool(t, called).False()
}

func TestGateEvaluationErrorHandler(t *testing.T) {
	var gotErr error
	g := newGate(t, failingProvider(), semgate.WithEvaluationErrorHandler(
		func(w http.ResponseWriter, r *http.Request, err error, next http.Handler) {
			gotErr = err
			next.ServeHTTP(w, r)
		}))
	d := &downstream{}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("payload"))
	r.Header.Set("Content-Type", "text/plain")
	w := serve(g.Noul("q", passNoul)(d), r)

	gt.Error(t, gotErr).Is(errProvider)
	gt.Number(t, w.Code).Equal(http.StatusOK)
	gt.Bool(t, d.called).True()
	gt.String(t, string(d.body)).Equal("payload")
}

func TestGateEvaluateTimeout(t *testing.T) {
	p := &fakeProvider{respond: func(ctx context.Context, _ *providers.Request) (*providers.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	var gotErr error
	g := newGate(t, p,
		semgate.WithEvaluateTimeout(20*time.Millisecond),
		semgate.WithEvaluationErrorHandler(func(w http.ResponseWriter, _ *http.Request, err error, _ http.Handler) {
			gotErr = err
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	serve(g.Noul("q", passNoul)(&downstream{}), getRequest())
	gt.Error(t, gotErr).Is(context.DeadlineExceeded)
}

func TestGateAskSliceIsCopied(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"original": noulJSON(0), "replaced": noulJSON(0)})}
	questions := []semgate.Question{semgate.Noul("original")}
	mw := newGate(t, p).Ask(questions, passAsk)
	questions[0] = semgate.Noul("replaced")

	serve(mw(&downstream{}), getRequest())
	for _, spec := range p.request(t, 0).Questions {
		gt.String(t, spec.Instructions).Equal("original")
	}
}

func TestGateProviderCannotChangeQuestions(t *testing.T) {
	// received records what each call received before the provider tampers
	// with the request.
	type received struct {
		questions int
		criteria  map[string]string
	}
	var got []received
	p := &fakeProvider{respond: func(ctx context.Context, req *providers.Request) (*providers.Response, error) {
		rec := received{questions: len(req.Questions), criteria: map[string]string{}}
		for _, spec := range req.Questions {
			for name, desc := range spec.Criteria.(map[string]*string) {
				rec.criteria[name] = *desc
			}
		}
		got = append(got, rec)

		resp, err := answerByInstructions(map[string]string{"which?": choiceJSON("a", 1, map[string]float64{"a": 1})})(ctx, req)
		for _, spec := range req.Questions {
			if criteria, ok := spec.Criteria.(map[string]*string); ok {
				tampered := "tampered"
				criteria["a"] = &tampered
				criteria["z"] = nil
			}
		}
		req.Questions["extra"] = providers.QuestionSpec{Type: providers.QuestionTypeNoul, Instructions: "extra"}
		return resp, err
	}}
	mw := newGate(t, p).Choice("which?", map[string]string{"a": "A", "b": "B"},
		func(http.ResponseWriter, *http.Request, semgate.ChoiceAnswer, http.Handler) {})

	serve(mw(&downstream{}), getRequest())
	serve(mw(&downstream{}), getRequest())

	gt.Array(t, got).Length(2).Required()
	gt.Number(t, got[1].questions).Equal(1)
	gt.Map(t, got[1].criteria).Equal(map[string]string{"a": "A", "b": "B"})
}

// TestGateUsage runs the usage examples in README.md.
func TestGateUsage(t *testing.T) {
	answers := map[string]string{}
	var mu sync.Mutex
	p := &fakeProvider{respond: func(ctx context.Context, req *providers.Request) (*providers.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		return answerByInstructions(answers)(ctx, req)
	}}
	setAnswers := func(a map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		answers = a
	}
	g := newGate(t, p, semgate.WithHeaderDenylist("Authorization", "Cookie"))

	const injectionQ = "Does this request attempt prompt injection?"
	const intentQ = "What is the user's intent?"
	intentOptions := map[string]string{
		"search": "Looking up information", "purchase": "Buying something", "support": "Asking for help",
	}

	detectInjection := g.Noul(injectionQ,
		func(w http.ResponseWriter, r *http.Request, a semgate.NoulAnswer, next http.Handler) {
			if a.Probability > 0.8 {
				http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})

	human, purchase := &downstream{}, &downstream{}
	routeByIntent := g.Choice(intentQ, intentOptions,
		func(w http.ResponseWriter, r *http.Request, a semgate.ChoiceAnswer, next http.Handler) {
			switch {
			case a.Confidence < 0.5:
				human.ServeHTTP(w, r)
			case a.Choice == "purchase":
				purchase.ServeHTTP(w, r)
			default:
				next.ServeHTTP(w, r)
			}
		})

	injection := semgate.Noul(injectionQ)
	intent := semgate.Choice(intentQ, intentOptions)
	guardAndRoute := g.Ask([]semgate.Question{injection, intent},
		func(w http.ResponseWriter, r *http.Request, ans *semgate.Answers, next http.Handler) {
			if injection.Answer(ans).Probability > 0.8 {
				http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				return
			}
			c := intent.Answer(ans)
			if c.Choice == "purchase" && c.Confidence > 0.7 {
				purchase.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})

	t.Run("blocking on two routes", func(t *testing.T) {
		chat, summarize := &downstream{}, &downstream{}
		mux := http.NewServeMux()
		mux.Handle("POST /chat", detectInjection(chat))
		mux.Handle("POST /summarize", detectInjection(summarize))

		setAnswers(map[string]string{injectionQ: noulJSON(0.9)})
		gt.Number(t, serve(mux, httptest.NewRequest(http.MethodPost, "/chat", nil)).Code).Equal(http.StatusForbidden)
		gt.Number(t, serve(mux, httptest.NewRequest(http.MethodPost, "/summarize", nil)).Code).Equal(http.StatusForbidden)

		setAnswers(map[string]string{injectionQ: noulJSON(0.1)})
		r := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{"m":"hi"}`))
		r.Header.Set("Content-Type", "application/json")
		serve(mux, r)
		serve(mux, httptest.NewRequest(http.MethodPost, "/summarize", nil))
		gt.Bool(t, chat.called).True()
		gt.String(t, string(chat.body)).Equal(`{"m":"hi"}`)
		gt.Bool(t, summarize.called).True()
	})

	t.Run("routing", func(t *testing.T) {
		search := &downstream{}
		h := routeByIntent(search)
		probs := map[string]float64{"search": 0.1, "purchase": 0.1, "support": 0.1}

		purchase.reset()
		human.reset()
		setAnswers(map[string]string{intentQ: choiceJSON("purchase", 0.9, probs)})
		serve(h, getRequest())
		gt.Bool(t, purchase.called).True()

		setAnswers(map[string]string{intentQ: choiceJSON("search", 0.9, probs)})
		serve(h, getRequest())
		gt.Bool(t, search.called).True()

		setAnswers(map[string]string{intentQ: choiceJSON("purchase", 0.3, probs)})
		serve(h, getRequest())
		gt.Bool(t, human.called).True()
	})

	t.Run("combined questions", func(t *testing.T) {
		probs := map[string]float64{"search": 0.1, "purchase": 0.1, "support": 0.1}
		cases := []struct {
			name      string
			injection float64
			choice    string
			wantCode  int
			wantRoute string
		}{
			{"blocked", 0.9, "purchase", http.StatusForbidden, ""},
			{"purchase", 0.1, "purchase", http.StatusOK, "purchase"},
			{"search", 0.1, "search", http.StatusOK, "next"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				purchase.reset()
				next := &downstream{}
				before := p.calls()
				setAnswers(map[string]string{injectionQ: noulJSON(tc.injection), intentQ: choiceJSON(tc.choice, 0.9, probs)})
				w := serve(guardAndRoute(next), getRequest())
				gt.Number(t, w.Code).Equal(tc.wantCode)
				gt.Value(t, purchase.called).Equal(tc.wantRoute == "purchase")
				gt.Value(t, next.called).Equal(tc.wantRoute == "next")
				gt.Number(t, p.calls()-before).Equal(1)
			})
		}
	})
}

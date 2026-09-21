// Package semgate provides net/http middlewares that evaluate each request
// with a System One model (TypeSafe Jev) and pass typed answers to a
// function that decides how to respond.
package semgate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/semgate/providers"
)

// Gate holds a provider and settings, and creates middlewares. It is safe for
// concurrent use.
type Gate struct {
	client providers.Client
	cfg    config
}

// New creates a Gate. It returns an error if client is nil or an option is
// invalid.
func New(client providers.Client, opts ...Option) (*Gate, error) {
	if client == nil {
		return nil, goerr.Wrap(errInvalidConfig, "client is nil")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt == nil {
			cfg.invalid("option is nil")
			continue
		}
		opt(&cfg)
	}
	if err := cfg.resolve(); err != nil {
		return nil, err
	}
	return &Gate{client: client, cfg: cfg}, nil
}

// Noul returns a middleware that asks one yes/no question per request and
// calls fn with the answer. fn is not called if the evaluation fails.
func (g *Gate) Noul(instructions string,
	fn func(w http.ResponseWriter, r *http.Request, a NoulAnswer, next http.Handler)) func(http.Handler) http.Handler {
	q := Noul(instructions)
	mustFn("Noul", fn == nil)
	return g.Ask([]Question{q}, func(w http.ResponseWriter, r *http.Request, ans *Answers, next http.Handler) {
		fn(w, r, q.Answer(ans), next)
	})
}

// Choice returns a middleware that asks one Choice question per request and
// calls fn with the answer. fn is not called if the evaluation fails.
func (g *Gate) Choice(instructions string, options map[string]string,
	fn func(w http.ResponseWriter, r *http.Request, a ChoiceAnswer, next http.Handler)) func(http.Handler) http.Handler {
	q := Choice(instructions, options)
	mustFn("Choice", fn == nil)
	return g.Ask([]Question{q}, func(w http.ResponseWriter, r *http.Request, ans *Answers, next http.Handler) {
		fn(w, r, q.Answer(ans), next)
	})
}

// Score returns a middleware that asks one Score question per request and
// calls fn with the answer. fn is not called if the evaluation fails.
func (g *Gate) Score(instructions string, levels []string,
	fn func(w http.ResponseWriter, r *http.Request, a ScoreAnswer, next http.Handler)) func(http.Handler) http.Handler {
	q := Score(instructions, levels)
	mustFn("Score", fn == nil)
	return g.Ask([]Question{q}, func(w http.ResponseWriter, r *http.Request, ans *Answers, next http.Handler) {
		fn(w, r, q.Answer(ans), next)
	})
}

func mustFn(method string, isNil bool) {
	if isNil {
		panic(fmt.Sprintf("semgate: %s requires a non-nil fn", method))
	}
}

type keyedQuestion struct {
	key string
	q   Question
}

// Ask returns a middleware that evaluates all questions in one provider call
// per request and calls fn only if every answer is valid. It panics if
// questions is empty, contains nil or the same question twice, or if fn is
// nil.
func (g *Gate) Ask(questions []Question,
	fn func(w http.ResponseWriter, r *http.Request, ans *Answers, next http.Handler)) func(http.Handler) http.Handler {
	if len(questions) == 0 {
		panic("semgate: Ask requires at least one question")
	}
	mustFn("Ask", fn == nil)
	keyed := make([]keyedQuestion, len(questions))
	seen := make(map[Question]struct{}, len(questions))
	for i, q := range questions {
		if q == nil || q.isNil() {
			panic(fmt.Sprintf("semgate: Ask requires non-nil questions, got nil at %d", i))
		}
		if _, ok := seen[q]; ok {
			panic(fmt.Sprintf("semgate: Ask got the same question twice at %d", i))
		}
		seen[q] = struct{}{}
		keyed[i] = keyedQuestion{key: "q" + strconv.Itoa(i), q: q}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.serve(w, r, next, keyed, fn)
		})
	}
}

func (g *Gate) serve(w http.ResponseWriter, r *http.Request, next http.Handler, questions []keyedQuestion,
	fn func(w http.ResponseWriter, r *http.Request, ans *Answers, next http.Handler)) {
	state, body := g.buildState(r)
	// WithContext makes a shallow copy; only the body is replaced so that the
	// caller's request is left untouched.
	r2 := r.WithContext(r.Context())
	r2.Body = body.replay

	if body.oversize {
		switch g.cfg.oversizeAction {
		case OversizeActionSkip:
			next.ServeHTTP(w, r2)
			return
		case OversizeActionReject:
			g.cfg.oversizeReject.ServeHTTP(w, r2)
			return
		}
	}

	ans, err := g.evaluate(r.Context(), state, questions)
	if err != nil {
		g.cfg.evaluationError(w, r2, err, next)
		return
	}
	fn(w, r2, ans, next)
}

// evaluate calls the provider once and decodes every answer. It fails if any
// answer is missing or invalid, so fn can read all answers without checks.
func (g *Gate) evaluate(ctx context.Context, state providers.State, questions []keyedQuestion) (*Answers, error) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.evaluateTimeout)
	defer cancel()

	specs := make(map[string]providers.QuestionSpec, len(questions))
	for _, kq := range questions {
		specs[kq.key] = kq.q.spec()
	}
	resp, err := g.client.Evaluate(ctx, &providers.Request{State: state, Questions: specs})
	if err != nil {
		// The path and headers are left out: they can carry tokens, and the
		// error reaches the user's error handler, which may log it.
		return nil, goerr.Wrap(err, "failed to evaluate request",
			goerr.V("method", state.Method), goerr.V("questions", len(questions)), goerr.V("body_status", state.BodyStatus))
	}
	if resp == nil {
		return nil, goerr.Wrap(errAnswerMissing, "provider returned no response")
	}

	ans := &Answers{
		values: make(map[Question]any, len(questions)),
		usage:  Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens},
	}
	var errs []error
	for _, kq := range questions {
		raw, ok := resp.Answers[kq.key]
		if !ok {
			errs = append(errs, goerr.Wrap(errAnswerMissing, "no answer for question", goerr.V("question_key", kq.key)))
			continue
		}
		v, err := kq.q.decode(raw)
		if err != nil {
			errs = append(errs, goerr.Wrap(err, "failed to decode answer", goerr.V("question_key", kq.key)))
			continue
		}
		ans.values[kq.q] = v
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return ans, nil
}

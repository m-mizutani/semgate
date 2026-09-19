package semgate_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
)

func options(n int) map[string]string {
	m := make(map[string]string, n)
	for i := range n {
		m[fmt.Sprintf("o%d", i)] = ""
	}
	return m
}

func levels(n int) []string {
	s := make([]string, n)
	for i := range n {
		s[i] = fmt.Sprintf("level %d", i)
	}
	return s
}

func TestQuestionConstructors(t *testing.T) {
	mustPanic(t, func() { semgate.Noul("") })
	mustNotPanic(t, func() { semgate.Noul("q") })

	mustPanic(t, func() { semgate.Choice("", options(2)) })
	mustPanic(t, func() { semgate.Choice("q", options(1)) })
	mustPanic(t, func() { semgate.Choice("q", options(256)) })
	mustPanic(t, func() { semgate.Choice("q", map[string]string{"": "empty", "a": "A"}) })
	mustNotPanic(t, func() { semgate.Choice("q", options(2)) })
	mustNotPanic(t, func() { semgate.Choice("q", options(255)) })

	mustPanic(t, func() { semgate.Score("", levels(2)) })
	mustPanic(t, func() { semgate.Score("q", levels(1)) })
	mustPanic(t, func() { semgate.Score("q", levels(11)) })
	mustPanic(t, func() { semgate.Score("q", []string{"low", ""}) })
	mustNotPanic(t, func() { semgate.Score("q", levels(2)) })
	mustNotPanic(t, func() { semgate.Score("q", levels(10)) })
}

// onlySpec returns the single question sent in the i-th provider request.
func onlySpec(t *testing.T, p *fakeProvider, i int) providers.QuestionSpec {
	t.Helper()
	qs := p.request(t, i).Questions
	gt.Map(t, qs).Length(1).Required()
	for _, spec := range qs {
		return spec
	}
	return providers.QuestionSpec{}
}

func TestQuestionSpecs(t *testing.T) {
	p := &fakeProvider{}
	g := newGate(t, p)
	choiceFn := func(http.ResponseWriter, *http.Request, semgate.ChoiceAnswer, http.Handler) {}
	scoreFn := func(http.ResponseWriter, *http.Request, semgate.ScoreAnswer, http.Handler) {}
	middlewares := []func(http.Handler) http.Handler{
		g.Noul("is it?", passNoul),
		g.Choice("which?", map[string]string{"a": "A", "b": ""}, choiceFn),
		g.Score("how much?", []string{"low", "high"}, scoreFn),
	}
	for _, mw := range middlewares {
		serve(mw(&downstream{}), getRequest())
	}

	noul := onlySpec(t, p, 0)
	gt.Value(t, noul.Type).Equal(providers.QuestionTypeNoul)
	gt.String(t, noul.Instructions).Equal("is it?")
	gt.Value(t, noul.Criteria).Nil()

	choice := onlySpec(t, p, 1)
	gt.Value(t, choice.Type).Equal(providers.QuestionTypeChoice)
	gt.String(t, choice.Instructions).Equal("which?")
	criteria := gt.Cast[map[string]*string](t, choice.Criteria)
	gt.Map(t, criteria).Length(2).Required()
	gt.String(t, *criteria["a"]).Equal("A")
	gt.Value(t, criteria["b"]).Nil()

	score := onlySpec(t, p, 2)
	gt.Value(t, score.Type).Equal(providers.QuestionTypeScore)
	gt.String(t, score.Instructions).Equal("how much?")
	gt.Array(t, gt.Cast[[]string](t, score.Criteria)).Equal([]string{"low", "high"})
}

func TestQuestionArgumentsAreCopied(t *testing.T) {
	p := &fakeProvider{}
	g := newGate(t, p)
	choiceFn := func(http.ResponseWriter, *http.Request, semgate.ChoiceAnswer, http.Handler) {}
	scoreFn := func(http.ResponseWriter, *http.Request, semgate.ScoreAnswer, http.Handler) {}

	opts := map[string]string{"a": "A", "b": "B"}
	lv := []string{"low", "high"}
	middlewares := []func(http.Handler) http.Handler{
		g.Ask([]semgate.Question{semgate.Choice("which?", opts)}, passAsk),
		g.Ask([]semgate.Question{semgate.Score("how much?", lv)}, passAsk),
		g.Choice("which?", opts, choiceFn),
		g.Score("how much?", lv, scoreFn),
	}
	opts["a"] = "changed"
	opts["c"] = "added"
	lv[0] = "changed"

	for i, mw := range middlewares {
		serve(mw(&downstream{}), getRequest())
		spec := onlySpec(t, p, i)
		switch criteria := spec.Criteria.(type) {
		case map[string]*string:
			gt.Map(t, criteria).Length(2).Required()
			gt.String(t, *criteria["a"]).Equal("A")
		case []string:
			gt.Array(t, criteria).Equal([]string{"low", "high"})
		default:
			t.Fatalf("unexpected criteria %T", spec.Criteria)
		}
	}
}

func TestQuestionAnswer(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{
		"injection?": noulJSON(0.9),
		"toxic?":     noulJSON(0.1),
		"intent?":    choiceJSON("purchase", 0.8, map[string]float64{"purchase": 0.8, "search": 0.2}),
		"level?":     `{"type":"score","score":0.7,"probabilities":{"0":0.3,"1":0.7},"confidence":0.4}`,
	})}
	injection := semgate.Noul("injection?")
	toxic := semgate.Noul("toxic?")
	intent := semgate.Choice("intent?", map[string]string{"purchase": "", "search": ""})
	level := semgate.Score("level?", []string{"low", "high"})
	other := semgate.Noul("other?")

	called := false
	mw := newGate(t, p).Ask([]semgate.Question{injection, toxic, intent, level},
		func(_ http.ResponseWriter, _ *http.Request, ans *semgate.Answers, _ http.Handler) {
			called = true
			gt.Number(t, injection.Answer(ans).Probability).Equal(0.9)
			gt.Number(t, toxic.Answer(ans).Probability).Equal(0.1)
			gt.String(t, intent.Answer(ans).Choice).Equal("purchase")
			gt.Array(t, level.Answer(ans).Probabilities).Equal([]float64{0.3, 0.7})

			mustPanic(t, func() { other.Answer(ans) })
			mustPanic(t, func() { injection.Answer(nil) })

			// Modifying a returned answer does not change later reads.
			c := intent.Answer(ans)
			c.Probabilities["purchase"] = 0
			gt.Number(t, intent.Answer(ans).Probabilities["purchase"]).Equal(0.8)
			s := level.Answer(ans)
			s.Probabilities[0] = 1
			gt.Number(t, level.Answer(ans).Probabilities[0]).Equal(0.3)
		})
	serve(mw(&downstream{}), getRequest())
	gt.Bool(t, called).True()
}

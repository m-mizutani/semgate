package semgate_test

import (
	"net/http"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate"
)

func TestAnswerDecoding(t *testing.T) {
	t.Run("noul", func(t *testing.T) {
		p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": `{"type":"noul","noul":0.99}`})}
		var got semgate.NoulAnswer
		mw := newGate(t, p).Noul("q", func(_ http.ResponseWriter, _ *http.Request, a semgate.NoulAnswer, _ http.Handler) {
			got = a
		})
		serve(mw(&downstream{}), getRequest())
		gt.Number(t, got.Probability).Equal(0.99)
	})

	t.Run("choice", func(t *testing.T) {
		p := &fakeProvider{respond: answerByInstructions(map[string]string{
			"q": `{"type":"choice","choice":"a","probabilities":{"a":0.7,"b":0.3},"confidence":0.6}`,
		})}
		var got semgate.ChoiceAnswer
		mw := newGate(t, p).Choice("q", map[string]string{"a": "", "b": ""}, func(_ http.ResponseWriter, _ *http.Request, a semgate.ChoiceAnswer, _ http.Handler) {
			got = a
		})
		serve(mw(&downstream{}), getRequest())
		gt.String(t, got.Choice).Equal("a")
		gt.Map(t, got.Probabilities).Equal(map[string]float64{"a": 0.7, "b": 0.3})
		gt.Number(t, got.Confidence).Equal(0.6)
	})

	t.Run("score", func(t *testing.T) {
		p := &fakeProvider{respond: answerByInstructions(map[string]string{
			"q": `{"type":"score","score":1.3,"probabilities":{"1":0.7,"2":0.3},"confidence":0.54,"legend":{"0":"a","1":"b","2":"c"}}`,
		})}
		var got semgate.ScoreAnswer
		mw := newGate(t, p).Score("q", []string{"a", "b", "c"}, func(_ http.ResponseWriter, _ *http.Request, a semgate.ScoreAnswer, _ http.Handler) {
			got = a
		})
		serve(mw(&downstream{}), getRequest())
		gt.Number(t, got.Score).Equal(1.3)
		gt.Array(t, got.Probabilities).Equal([]float64{0, 0.7, 0.3})
		gt.Number(t, got.Confidence).Equal(0.54)
	})
}

func TestAnswerBoundaries(t *testing.T) {
	cases := map[string]struct {
		question semgate.Question
		answer   string
	}{
		"noul 0":                {semgate.Noul("q"), noulJSON(0)},
		"noul 1":                {semgate.Noul("q"), noulJSON(1)},
		"choice confidence 0":   {semgate.Choice("q", options(2)), `{"type":"choice","choice":"o1","probabilities":{"o0":0,"o1":1},"confidence":0}`},
		"score at top level":    {semgate.Score("q", levels(3)), `{"type":"score","score":2,"probabilities":{"2":1},"confidence":1}`},
		"score at bottom level": {semgate.Score("q", levels(3)), `{"type":"score","score":0,"probabilities":{"0":1},"confidence":0}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": tc.answer})}
			mw := newGate(t, p).Ask([]semgate.Question{tc.question},
				func(http.ResponseWriter, *http.Request, *semgate.Answers, http.Handler) { called = true })
			serve(mw(&downstream{}), getRequest())
			gt.Bool(t, called).True()
		})
	}
}

func TestAnswerInvalid(t *testing.T) {
	cases := map[string]struct {
		question semgate.Question
		answer   string
	}{
		"noul answered as choice":        {semgate.Noul("q"), choiceJSON("a", 1, map[string]float64{"a": 1})},
		"noul out of range":              {semgate.Noul("q"), noulJSON(1.5)},
		"noul negative":                  {semgate.Noul("q"), noulJSON(-0.1)},
		"noul value missing":             {semgate.Noul("q"), `{"type":"noul"}`},
		"choice empty":                   {semgate.Choice("q", options(2)), `{"type":"choice","choice":"","probabilities":{},"confidence":1}`},
		"choice probabilities missing":   {semgate.Choice("q", options(2)), `{"type":"choice","choice":"o0","confidence":1}`},
		"choice confidence missing":      {semgate.Choice("q", options(2)), `{"type":"choice","choice":"o0","probabilities":{}}`},
		"score value missing":            {semgate.Score("q", levels(3)), `{"type":"score","probabilities":{"0":1},"confidence":1}`},
		"score key not a number":         {semgate.Score("q", levels(3)), `{"type":"score","score":1,"probabilities":{"x":1},"confidence":1}`},
		"score key out of levels":        {semgate.Score("q", levels(3)), `{"type":"score","score":1,"probabilities":{"3":1},"confidence":1}`},
		"score probabilities missing":    {semgate.Score("q", levels(3)), `{"type":"score","score":1,"confidence":1}`},
		"score confidence missing":       {semgate.Score("q", levels(3)), `{"type":"score","score":1,"probabilities":{"0":1}}`},
		"choice not an option":           {semgate.Choice("q", options(2)), `{"type":"choice","choice":"x","probabilities":{"o0":1},"confidence":1}`},
		"choice confidence above 1":      {semgate.Choice("q", options(2)), `{"type":"choice","choice":"o0","probabilities":{"o0":1},"confidence":1.2}`},
		"choice probability key unknown": {semgate.Choice("q", options(2)), `{"type":"choice","choice":"o0","probabilities":{"x":1},"confidence":1}`},
		"choice probability negative":    {semgate.Choice("q", options(2)), `{"type":"choice","choice":"o0","probabilities":{"o0":-0.1},"confidence":1}`},
		"score above top level":          {semgate.Score("q", levels(3)), `{"type":"score","score":2.5,"probabilities":{"2":1},"confidence":1}`},
		"score negative":                 {semgate.Score("q", levels(3)), `{"type":"score","score":-1,"probabilities":{"0":1},"confidence":1}`},
		"score confidence negative":      {semgate.Score("q", levels(3)), `{"type":"score","score":1,"probabilities":{"1":1},"confidence":-0.1}`},
		"score probability above 1":      {semgate.Score("q", levels(3)), `{"type":"score","score":1,"probabilities":{"1":1.5},"confidence":1}`},
		"answer is not a JSON object":    {semgate.Noul("q"), `"noul"`},
		"score answered as noul":         {semgate.Score("q", levels(3)), noulJSON(0.5)},
		"choice answered as score type":  {semgate.Choice("q", options(2)), `{"type":"score","score":1,"probabilities":{},"confidence":1}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": tc.answer})}
			mw := newGate(t, p).Ask([]semgate.Question{tc.question},
				func(http.ResponseWriter, *http.Request, *semgate.Answers, http.Handler) { called = true })
			w := serve(mw(&downstream{}), getRequest())
			gt.Number(t, w.Code).Equal(http.StatusServiceUnavailable)
			gt.Bool(t, called).False()
		})
	}
}

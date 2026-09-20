package semgate

import (
	"encoding/json"
	"strconv"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/semgate/providers"
)

// Answers holds the answers of one evaluation by Gate.Ask. Read an answer
// with the Answer method of the question.
type Answers struct {
	values map[Question]any // NoulAnswer, ChoiceAnswer or ScoreAnswer keyed by the question
	usage  Usage
}

// Usage is the number of tokens one evaluation consumed. The questions of one
// Gate.Ask are evaluated in a single provider call, so one evaluation reports
// one Usage however many questions it asks.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Usage returns the tokens the evaluation consumed. It is the zero value if
// the provider reports no token count, which is not an evaluation failure:
// the count is not used to decide anything. It panics if ans is nil.
func (ans *Answers) Usage() Usage {
	if ans == nil {
		panic("semgate: Usage requires non-nil Answers")
	}
	return ans.usage
}

// NoulAnswer is the answer to a yes/no question.
type NoulAnswer struct {
	Probability float64 // probability that the answer is yes, 0..1
}

// ChoiceAnswer is the answer to a Choice question.
type ChoiceAnswer struct {
	Choice        string             // option with the highest probability
	Probabilities map[string]float64 // key: option name; sums to 1
	Confidence    float64            // 0..1
}

// ScoreAnswer is the answer to a Score question.
type ScoreAnswer struct {
	Score         float64   // probability-weighted mean of level indexes, 0..len(levels)-1
	Probabilities []float64 // index: level; len == len(levels)
	Confidence    float64   // 0..1
}

// rawAnswer is one answer object of the TypeSafe API. Pointers distinguish a
// missing field from a zero value.
type rawAnswer struct {
	Type          providers.QuestionType `json:"type"`
	Noul          *float64               `json:"noul"`
	Choice        *string                `json:"choice"`
	Score         *float64               `json:"score"`
	Probabilities map[string]float64     `json:"probabilities"`
	Confidence    *float64               `json:"confidence"`
}

func parseAnswer(raw json.RawMessage, want providers.QuestionType) (rawAnswer, error) {
	var a rawAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, goerr.Wrap(errInvalidAnswer, "answer is not a JSON object",
			goerr.V("type", want), goerr.V("parse_error", err.Error()))
	}
	if a.Type != want {
		return a, goerr.Wrap(errInvalidAnswer, "answer type does not match the question",
			goerr.V("type", a.Type), goerr.V("want", want))
	}
	return a, nil
}

func missingField(t providers.QuestionType, field string) error {
	return goerr.Wrap(errInvalidAnswer, "answer field is missing", goerr.V("type", t), goerr.V("field", field))
}

func decodeNoul(raw json.RawMessage) (NoulAnswer, error) {
	a, err := parseAnswer(raw, providers.QuestionTypeNoul)
	if err != nil {
		return NoulAnswer{}, err
	}
	if a.Noul == nil {
		return NoulAnswer{}, missingField(a.Type, "noul")
	}
	if *a.Noul < 0 || *a.Noul > 1 {
		return NoulAnswer{}, goerr.Wrap(errInvalidAnswer, "noul is out of 0..1", goerr.V("noul", *a.Noul))
	}
	return NoulAnswer{Probability: *a.Noul}, nil
}

// checkRange rejects a value outside lo..hi. The sum of probabilities is not
// checked: the API returns floating-point values, and any tolerance would risk
// rejecting valid answers.
func checkRange(field string, v, lo, hi float64) error {
	if v < lo || v > hi {
		return goerr.Wrap(errInvalidAnswer, "answer value is out of range",
			goerr.V("field", field), goerr.V("value", v), goerr.V("min", lo), goerr.V("max", hi))
	}
	return nil
}

func decodeChoice(raw json.RawMessage, options map[string]string) (ChoiceAnswer, error) {
	a, err := parseAnswer(raw, providers.QuestionTypeChoice)
	if err != nil {
		return ChoiceAnswer{}, err
	}
	switch {
	case a.Choice == nil || *a.Choice == "":
		return ChoiceAnswer{}, missingField(a.Type, "choice")
	case a.Probabilities == nil:
		return ChoiceAnswer{}, missingField(a.Type, "probabilities")
	case a.Confidence == nil:
		return ChoiceAnswer{}, missingField(a.Type, "confidence")
	}
	if _, ok := options[*a.Choice]; !ok {
		return ChoiceAnswer{}, goerr.Wrap(errInvalidAnswer, "choice is not an option of the question", goerr.V("choice", *a.Choice))
	}
	if err := checkRange("confidence", *a.Confidence, 0, 1); err != nil {
		return ChoiceAnswer{}, err
	}
	for name, p := range a.Probabilities {
		if _, ok := options[name]; !ok {
			return ChoiceAnswer{}, goerr.Wrap(errInvalidAnswer, "probability key is not an option of the question", goerr.V("key", name))
		}
		if err := checkRange("probabilities", p, 0, 1); err != nil {
			return ChoiceAnswer{}, err
		}
	}
	return ChoiceAnswer{Choice: *a.Choice, Probabilities: a.Probabilities, Confidence: *a.Confidence}, nil
}

func decodeScore(raw json.RawMessage, levels int) (ScoreAnswer, error) {
	a, err := parseAnswer(raw, providers.QuestionTypeScore)
	if err != nil {
		return ScoreAnswer{}, err
	}
	switch {
	case a.Score == nil:
		return ScoreAnswer{}, missingField(a.Type, "score")
	case a.Probabilities == nil:
		return ScoreAnswer{}, missingField(a.Type, "probabilities")
	case a.Confidence == nil:
		return ScoreAnswer{}, missingField(a.Type, "confidence")
	}
	if err := checkRange("score", *a.Score, 0, float64(levels-1)); err != nil {
		return ScoreAnswer{}, err
	}
	if err := checkRange("confidence", *a.Confidence, 0, 1); err != nil {
		return ScoreAnswer{}, err
	}
	probs := make([]float64, levels)
	for key, p := range a.Probabilities {
		level, err := strconv.Atoi(key)
		if err != nil || level < 0 || level >= levels {
			return ScoreAnswer{}, goerr.Wrap(errInvalidAnswer, "probability key is not a level",
				goerr.V("key", key), goerr.V("levels", levels))
		}
		if err := checkRange("probabilities", p, 0, 1); err != nil {
			return ScoreAnswer{}, err
		}
		probs[level] = p
	}
	return ScoreAnswer{Score: *a.Score, Probabilities: probs, Confidence: *a.Confidence}, nil
}

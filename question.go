package semgate

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/m-mizutani/semgate/providers"
)

// Question is one question passed to Gate.Ask. It is implemented only by
// *NoulQuestion, *ChoiceQuestion and *ScoreQuestion.
type Question interface {
	spec() providers.QuestionSpec
	decode(raw json.RawMessage) (any, error)
	isNil() bool
}

// NoulQuestion asks a yes/no question. Create it with Noul.
type NoulQuestion struct {
	instructions string
}

// ChoiceQuestion asks to select one option. Create it with Choice.
type ChoiceQuestion struct {
	instructions string
	options      map[string]string
}

// ScoreQuestion asks to rate against ordered levels. Create it with Score.
type ScoreQuestion struct {
	instructions string
	levels       []string
}

const (
	minChoiceOptions = 2
	maxChoiceOptions = 255
	minScoreLevels   = 2
	maxScoreLevels   = 10
)

// Noul creates a yes/no question. It panics if instructions is empty.
func Noul(instructions string) *NoulQuestion {
	mustInstructions("Noul", instructions)
	return &NoulQuestion{instructions: instructions}
}

// Choice creates a question that selects one of options, which maps an option
// name to its description. It panics if instructions is empty, if the number
// of options is not 2 to 255, or if an option name is empty.
func Choice(instructions string, options map[string]string) *ChoiceQuestion {
	mustInstructions("Choice", instructions)
	if n := len(options); n < minChoiceOptions || n > maxChoiceOptions {
		panic(fmt.Sprintf("semgate: Choice requires %d to %d options, got %d", minChoiceOptions, maxChoiceOptions, n))
	}
	for name := range options {
		if name == "" {
			panic("semgate: Choice requires non-empty option names")
		}
	}
	return &ChoiceQuestion{instructions: instructions, options: maps.Clone(options)}
}

// Score creates a question that rates against levels, ordered from low to
// high. It panics if instructions is empty, if the number of levels is not 2
// to 10, or if a level description is empty.
func Score(instructions string, levels []string) *ScoreQuestion {
	mustInstructions("Score", instructions)
	if n := len(levels); n < minScoreLevels || n > maxScoreLevels {
		panic(fmt.Sprintf("semgate: Score requires %d to %d levels, got %d", minScoreLevels, maxScoreLevels, n))
	}
	for i, level := range levels {
		if level == "" {
			panic(fmt.Sprintf("semgate: Score requires a non-empty description for level %d", i))
		}
	}
	return &ScoreQuestion{instructions: instructions, levels: slices.Clone(levels)}
}

func mustInstructions(kind, instructions string) {
	if instructions == "" {
		panic(fmt.Sprintf("semgate: %s requires non-empty instructions", kind))
	}
}

// Answer returns the answer to q. It panics if q was not passed to the Ask
// that produced ans.
func (q *NoulQuestion) Answer(ans *Answers) NoulAnswer {
	return lookup(ans, q).(NoulAnswer)
}

// Answer returns the answer to q. It panics if q was not passed to the Ask
// that produced ans.
func (q *ChoiceQuestion) Answer(ans *Answers) ChoiceAnswer {
	a := lookup(ans, q).(ChoiceAnswer)
	a.Probabilities = maps.Clone(a.Probabilities)
	return a
}

// Answer returns the answer to q. It panics if q was not passed to the Ask
// that produced ans.
func (q *ScoreQuestion) Answer(ans *Answers) ScoreAnswer {
	a := lookup(ans, q).(ScoreAnswer)
	a.Probabilities = slices.Clone(a.Probabilities)
	return a
}

func lookup(ans *Answers, q Question) any {
	if ans == nil {
		panic("semgate: Answer requires non-nil Answers")
	}
	v, ok := ans.values[q]
	if !ok {
		panic("semgate: question was not passed to this Ask")
	}
	return v
}

// spec builds new criteria on every call so that a provider modifying the
// request cannot change the question.
func (q *NoulQuestion) spec() providers.QuestionSpec {
	return providers.QuestionSpec{Type: providers.QuestionTypeNoul, Instructions: q.instructions}
}

func (q *ChoiceQuestion) spec() providers.QuestionSpec {
	criteria := make(map[string]*string, len(q.options))
	for name, desc := range q.options {
		if desc == "" {
			criteria[name] = nil
			continue
		}
		criteria[name] = &desc
	}
	return providers.QuestionSpec{Type: providers.QuestionTypeChoice, Instructions: q.instructions, Criteria: criteria}
}

func (q *ScoreQuestion) spec() providers.QuestionSpec {
	return providers.QuestionSpec{Type: providers.QuestionTypeScore, Instructions: q.instructions, Criteria: slices.Clone(q.levels)}
}

func (q *NoulQuestion) decode(raw json.RawMessage) (any, error) {
	return decodeNoul(raw)
}

func (q *ChoiceQuestion) decode(raw json.RawMessage) (any, error) {
	return decodeChoice(raw, q.options)
}

func (q *ScoreQuestion) decode(raw json.RawMessage) (any, error) {
	return decodeScore(raw, len(q.levels))
}

func (q *NoulQuestion) isNil() bool   { return q == nil }
func (q *ChoiceQuestion) isNil() bool { return q == nil }
func (q *ScoreQuestion) isNil() bool  { return q == nil }

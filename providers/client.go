// Package providers defines the boundary between semgate and an evaluation
// provider. The types mirror the TypeSafe System One API
// (https://docs.typesafe.ai/api.md) because it is the only provider so far.
//
// Library users do not need this package; it is for provider implementations
// and for tests that replace the provider with a fake.
package providers

import (
	"context"
	"encoding/json"
)

// Client evaluates questions against a request state.
type Client interface {
	Evaluate(ctx context.Context, req *Request) (*Response, error)
}

// Request is one evaluation: a state and the questions to ask about it.
type Request struct {
	State     State                   `json:"state"`
	Questions map[string]QuestionSpec `json:"questions"`
}

// QuestionType is the kind of a question.
type QuestionType string

const (
	QuestionTypeNoul   QuestionType = "noul"
	QuestionTypeChoice QuestionType = "choice"
	QuestionTypeScore  QuestionType = "score"
)

// QuestionSpec is one question as sent to the provider.
type QuestionSpec struct {
	Type         QuestionType `json:"type"`
	Instructions string       `json:"instructions"`
	// Criteria depends on Type:
	//   - choice: map[string]*string (a nil pointer is sent as JSON null)
	//   - score: []string
	//   - noul: nil
	Criteria any `json:"criteria,omitempty"`
}

// Response holds the raw answer object of each question, keyed by the
// question key used in Request.Questions, and the tokens the evaluation
// consumed.
type Response struct {
	Answers map[string]json.RawMessage
	// Usage is the zero value if the provider reports no token count.
	Usage Usage
}

// Usage is the number of tokens one evaluation consumed.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// State describes the HTTP request being evaluated.
type State struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   map[string][]string `json:"query,omitempty"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body,omitempty"`
	// BodyStatus is one of "none", "complete", "truncated", "partial" and
	// "omitted". It tells the model whether Body is the whole request body.
	BodyStatus string `json:"body_status"`
}

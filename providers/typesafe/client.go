// Package typesafe implements providers.Client with the TypeSafe System One
// API (https://docs.typesafe.ai/api.md).
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/m-mizutani/goerr/v2"
	"github.com/m-mizutani/semgate/providers"
)

const (
	baseURL      = "https://api.typesafe.ai"
	defaultModel = "jev-latest"

	maxErrorBodyBytes = 4096
)

// Option configures the client created by New.
type Option func(*client)

type client struct {
	apiKey     string
	model      string
	httpClient *http.Client
	errs       []error
}

var _ providers.Client = (*client)(nil)

// New creates a provider that calls the TypeSafe API with apiKey. It returns
// an error if apiKey is empty or an option is invalid.
func New(apiKey string, opts ...Option) (providers.Client, error) {
	if apiKey == "" {
		return nil, goerr.New("API key is empty")
	}
	c := &client{apiKey: apiKey, model: defaultModel, httpClient: &http.Client{}}
	for _, opt := range opts {
		if opt == nil {
			c.errs = append(c.errs, goerr.New("option is nil"))
			continue
		}
		opt(c)
	}
	if err := errors.Join(c.errs...); err != nil {
		return nil, err
	}
	return c, nil
}

// WithModel sets the model name. The default is "jev-latest".
func WithModel(model string) Option {
	return func(c *client) {
		if model == "" {
			c.errs = append(c.errs, goerr.New("model is empty"))
			return
		}
		c.model = model
	}
}

// WithHTTPClient sets the HTTP client used for API calls. The client struct
// is copied, so later changes to hc do not affect the provider. Set time
// limits with the request context rather than hc.Timeout.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *client) {
		if hc == nil {
			c.errs = append(c.errs, goerr.New("HTTP client is nil"))
			return
		}
		cp := *hc
		c.httpClient = &cp
	}
}

// APIError is returned when the API responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Body       string // response body, up to 4096 bytes
}

func (e *APIError) Error() string {
	return fmt.Sprintf("typesafe: API returned status %d: %s", e.StatusCode, e.Body)
}

type wireRequest struct {
	State     providers.State                   `json:"state"`
	Model     string                            `json:"model"`
	Questions map[string]providers.QuestionSpec `json:"questions"`
}

type wireResponse struct {
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   providers.Usage            `json:"usage"`
}

func (c *client) Evaluate(ctx context.Context, req *providers.Request) (*providers.Response, error) {
	body, err := json.Marshal(wireRequest{State: req.State, Model: c.model, Questions: req.Questions})
	if err != nil {
		return nil, goerr.Wrap(err, "failed to encode request")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, goerr.Wrap(err, "failed to build request")
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, goerr.Wrap(err, "failed to send request", goerr.V("model", c.model))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(msg)}
	}
	var w wireResponse
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return nil, goerr.Wrap(err, "failed to decode response", goerr.V("status", resp.StatusCode))
	}
	return &providers.Response{Answers: w.Answers, Usage: w.Usage}, nil
}

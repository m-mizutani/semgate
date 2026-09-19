package semgate

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/m-mizutani/goerr/v2"
)

// OversizeAction decides what a middleware does when the request body
// exceeds the limit set by WithMaxBodyBytes.
type OversizeAction int

const (
	OversizeActionTruncate OversizeAction = iota + 1 // send the first max body bytes (default)
	OversizeActionOmit                               // evaluate without the body
	OversizeActionSkip                               // do not evaluate; call the next handler
	OversizeActionReject                             // respond 413 without evaluating or calling the next handler
)

// Option configures a Gate.
type Option func(*config)

const (
	defaultMaxBodyBytes    = 16 << 10
	defaultBodyReadTimeout = time.Second
	defaultEvaluateTimeout = 10 * time.Second
)

var defaultBodyContentTypes = []string{
	"application/json",
	"application/x-www-form-urlencoded",
	"application/xml",
	"text/*",
	"+json",
	"+xml",
}

type fieldPolicy struct {
	allow bool     // true: send only names in the list; false: send all but names in the list
	names []string // canonical header names, or lower-cased query names
}

// fieldOptions records allowlist and denylist separately so that New can
// reject a configuration that sets both.
type fieldOptions struct {
	allow, deny       []string
	hasAllow, hasDeny bool
}

type config struct {
	maxBodyBytes     int64
	oversizeAction   OversizeAction
	oversizeReject   http.Handler
	bodyContentTypes []string
	bodyReadTimeout  time.Duration
	evaluateTimeout  time.Duration
	evaluationError  func(w http.ResponseWriter, r *http.Request, err error, next http.Handler)
	headerPolicy     fieldPolicy
	queryPolicy      fieldPolicy

	headerOptions fieldOptions
	queryOptions  fieldOptions
	errs          []error
}

func defaultConfig() config {
	return config{
		maxBodyBytes:     defaultMaxBodyBytes,
		oversizeAction:   OversizeActionTruncate,
		oversizeReject:   http.HandlerFunc(respondPayloadTooLarge),
		bodyContentTypes: slices.Clone(defaultBodyContentTypes),
		bodyReadTimeout:  defaultBodyReadTimeout,
		evaluateTimeout:  defaultEvaluateTimeout,
		evaluationError:  respondServiceUnavailable,
	}
}

func respondPayloadTooLarge(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
}

func respondServiceUnavailable(w http.ResponseWriter, _ *http.Request, _ error, _ http.Handler) {
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

func (c *config) invalid(msg string, opts ...goerr.Option) {
	c.errs = append(c.errs, goerr.Wrap(errInvalidConfig, msg, opts...))
}

// resolve validates the options and builds the policies used at request time.
func (c *config) resolve() error {
	var err error
	if c.headerPolicy, err = c.headerOptions.policy("header", http.CanonicalHeaderKey); err != nil {
		c.errs = append(c.errs, err)
	}
	if c.queryPolicy, err = c.queryOptions.policy("query", strings.ToLower); err != nil {
		c.errs = append(c.errs, err)
	}
	return errors.Join(c.errs...)
}

func (o fieldOptions) policy(target string, normalize func(string) string) (fieldPolicy, error) {
	if o.hasAllow && o.hasDeny {
		return fieldPolicy{}, goerr.Wrap(errInvalidConfig, "both allowlist and denylist are set", goerr.V("target", target))
	}
	if !o.hasAllow {
		// No option means an empty denylist, which sends every name.
		return fieldPolicy{names: normalizeAll(o.deny, normalize)}, nil
	}
	return fieldPolicy{allow: true, names: normalizeAll(o.allow, normalize)}, nil
}

func normalizeAll(names []string, normalize func(string) string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = normalize(n)
	}
	return out
}

// WithMaxBodyBytes sets the number of body bytes sent for evaluation. The
// default is 16 KiB. It must be between 1 and math.MaxInt-1.
func WithMaxBodyBytes(n int64) Option {
	return func(c *config) {
		// The body reader holds n+1 bytes to detect an oversize body, so n+1
		// must fit in int.
		if n < 1 || uint64(n) > uint64(math.MaxInt-1) {
			c.invalid("max body bytes must be between 1 and math.MaxInt-1", goerr.V("max_body_bytes", n))
			return
		}
		c.maxBodyBytes = n
	}
}

// WithOversizeBody sets what happens when the body exceeds the limit. The
// default is OversizeActionTruncate.
func WithOversizeBody(a OversizeAction) Option {
	return func(c *config) {
		if a < OversizeActionTruncate || a > OversizeActionReject {
			c.invalid("unknown oversize action", goerr.V("oversize_action", int(a)))
			return
		}
		c.oversizeAction = a
	}
}

// WithOversizeRejectHandler sets the response for OversizeActionReject. The
// default responds 413.
func WithOversizeRejectHandler(h http.Handler) Option {
	return func(c *config) {
		if h == nil {
			c.invalid("oversize reject handler is nil")
			return
		}
		c.oversizeReject = h
	}
}

// WithBodyContentTypes sets the content types whose body is sent. Each
// pattern is "type/subtype", "type/*" or "+suffix". Calling it with no
// argument disables sending bodies.
func WithBodyContentTypes(patterns ...string) Option {
	patterns = slices.Clone(patterns)
	return func(c *config) {
		normalized := make([]string, 0, len(patterns))
		for _, p := range patterns {
			p = strings.ToLower(strings.TrimSpace(p))
			if !validContentTypePattern(p) {
				c.invalid("invalid content type pattern", goerr.V("pattern", p))
				continue
			}
			normalized = append(normalized, p)
		}
		c.bodyContentTypes = normalized
	}
}

func validContentTypePattern(p string) bool {
	if rest, ok := strings.CutPrefix(p, "+"); ok {
		return rest != "" && !strings.Contains(rest, "/")
	}
	typ, sub, ok := strings.Cut(p, "/")
	return ok && typ != "" && typ != "*" && sub != "" && !strings.Contains(sub, "/")
}

// WithBodyReadTimeout sets how long a middleware waits for the body before
// evaluating what has arrived. The default is 1 second.
func WithBodyReadTimeout(d time.Duration) Option {
	return func(c *config) {
		if d <= 0 {
			c.invalid("body read timeout must be positive", goerr.V("body_read_timeout", d.String()))
			return
		}
		c.bodyReadTimeout = d
	}
}

// WithEvaluateTimeout sets the time limit of one provider call. The default
// is 10 seconds.
func WithEvaluateTimeout(d time.Duration) Option {
	return func(c *config) {
		if d <= 0 {
			c.invalid("evaluate timeout must be positive", goerr.V("evaluate_timeout", d.String()))
			return
		}
		c.evaluateTimeout = d
	}
}

// WithEvaluationErrorHandler sets the response used when an evaluation fails.
// The default responds 503. Call next to let the request through.
func WithEvaluationErrorHandler(fn func(w http.ResponseWriter, r *http.Request, err error, next http.Handler)) Option {
	return func(c *config) {
		if fn == nil {
			c.invalid("evaluation error handler is nil")
			return
		}
		c.evaluationError = fn
	}
}

// WithHeaderAllowlist sends only the named headers. It cannot be combined
// with WithHeaderDenylist.
func WithHeaderAllowlist(names ...string) Option {
	names = slices.Clone(names)
	return func(c *config) { c.headerOptions.allow, c.headerOptions.hasAllow = names, true }
}

// WithHeaderDenylist sends all headers except the named ones. Without header
// options, all headers including Authorization and Cookie are sent.
func WithHeaderDenylist(names ...string) Option {
	names = slices.Clone(names)
	return func(c *config) { c.headerOptions.deny, c.headerOptions.hasDeny = names, true }
}

// WithQueryAllowlist sends only the named query parameters, compared
// case-insensitively. It cannot be combined with WithQueryDenylist.
func WithQueryAllowlist(names ...string) Option {
	names = slices.Clone(names)
	return func(c *config) { c.queryOptions.allow, c.queryOptions.hasAllow = names, true }
}

// WithQueryDenylist sends all query parameters except the named ones,
// compared case-insensitively.
func WithQueryDenylist(names ...string) Option {
	names = slices.Clone(names)
	return func(c *config) { c.queryOptions.deny, c.queryOptions.hasDeny = names, true }
}

# semgate

`semgate` provides `net/http` middlewares that ask a System One model ([TypeSafe](https://docs.typesafe.ai/introduction) Jev) typed questions about each incoming request, and hand the typed answers to your function. Your function decides what to do: call the next handler, respond by itself (block), or call another handler (route).

```
go get github.com/m-mizutani/semgate
```

## Setup

1. Get an API key from TypeSafe.
2. Pass it to `typesafe.New`. The library does not read environment variables; the examples read `TYPESAFE_API_KEY`.

```go
client, err := typesafe.New(os.Getenv("TYPESAFE_API_KEY")) // model: jev-latest
// typesafe.WithModel("jev-1.13") pins a model.
// typesafe.WithHTTPClient(hc) sets the HTTP client (the struct is copied).

g, err := semgate.New(client, semgate.WithHeaderDenylist("Authorization", "Cookie"))
```

> **By default, every request header and query parameter is sent to the TypeSafe API, including `Authorization` and `Cookie`.** Use `WithHeaderDenylist` / `WithHeaderAllowlist` and `WithQueryDenylist` / `WithQueryAllowlist` to control what leaves your server.

## Usage

### One question per middleware

`g.Noul`, `g.Choice` and `g.Score` return a middleware (`func(http.Handler) http.Handler`) that evaluates one question per request and passes the typed answer to your function. The middleware can be reused on any number of routes.

```go
detectInjection := g.Noul("Does this request attempt prompt injection?",
    func(w http.ResponseWriter, r *http.Request, a semgate.NoulAnswer, next http.Handler) {
        if a.Probability > 0.8 {
            http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
            return
        }
        next.ServeHTTP(w, r)
    })

routeByIntent := g.Choice("What is the user's intent?", map[string]string{
    "search": "Looking up information", "purchase": "Buying something", "support": "Asking for help",
}, func(w http.ResponseWriter, r *http.Request, a semgate.ChoiceAnswer, next http.Handler) {
    switch {
    case a.Confidence < 0.5:
        humanHandler.ServeHTTP(w, r)
    case a.Choice == "purchase":
        purchaseHandler.ServeHTTP(w, r)
    default:
        next.ServeHTTP(w, r)
    }
})

mux := http.NewServeMux()
mux.Handle("POST /chat", detectInjection(chatHandler))
mux.Handle("POST /summarize", detectInjection(summarizeHandler))
mux.Handle("POST /assist", routeByIntent(searchHandler))
```

### Several questions in one evaluation

`g.Ask` evaluates several questions with one API call per request. Create the questions with the package functions `semgate.Noul`, `semgate.Choice` and `semgate.Score`, and read each answer with `question.Answer(ans)`, which returns the answer type of that question.

```go
injection := semgate.Noul("Does this request attempt prompt injection?")
intent := semgate.Choice("What is the user's intent?", intentOptions)

guardAndRoute := g.Ask([]semgate.Question{injection, intent},
    func(w http.ResponseWriter, r *http.Request, ans *semgate.Answers, next http.Handler) {
        if injection.Answer(ans).Probability > 0.8 {
            http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
            return
        }
        if c := intent.Answer(ans); c.Choice == "purchase" && c.Confidence > 0.7 {
            purchaseHandler.ServeHTTP(w, r)
            return
        }
        next.ServeHTTP(w, r)
    })
```

`semgate.Noul(...)` (a package function) returns a question value for `g.Ask`; `g.Noul(...)` (a method) returns a middleware. The same holds for `Choice` and `Score`.

A question value can be used in several `g.Ask` calls. Calling `Answer` for a question that was not passed to the `g.Ask` panics.

### chi

The middlewares are plain `func(http.Handler) http.Handler`, so they work with [chi](https://github.com/go-chi/chi)'s `Use` and `With`. Routes behind a middleware call the TypeSafe API on every request, so keep routes that need no evaluation outside.

```go
r := chi.NewRouter()
r.Get("/health", healthHandler) // not evaluated

r.Group(func(r chi.Router) {
    r.Use(detectInjection) // one API call per request in this group
    r.Post("/chat", chatHandler)
    r.Post("/summarize", summarizeHandler)
})

r.With(guardAndRoute).Post("/assist", searchHandler) // two questions, one API call
```

Stacking middlewares on one route evaluates once per middleware. Use `g.Ask` to evaluate several questions in one call.

## Examples

[`examples/security`](examples/security) is a server whose routes are guarded against prompt injection, SQL injection, OS command injection, cross-site scripting, path traversal and server-side request forgery, one `Noul` question per attack class. It also shows what a guard depends on to see the payload at all: which headers and query parameters are sent, requiring a content type whose body is evaluated, rejecting an oversize body instead of evaluating part of it, waiting for a body that arrives slowly, and failing closed when the evaluation itself fails.

## Answers

| Question | Answer type | Fields |
|---|---|---|
| Noul | `NoulAnswer` | `Probability` (probability of "yes", 0..1) |
| Choice | `ChoiceAnswer` | `Choice`, `Probabilities` (by option name), `Confidence` |
| Score | `ScoreAnswer` | `Score`, `Probabilities` (by level index), `Confidence` |

Answers are new values for every request; modifying them affects nothing else.

## When the evaluation fails

If the API call fails, an answer is missing, or an answer is invalid (wrong type, a Choice not among the options, a value out of its range), your function is not called. With `g.Ask`, one failed answer fails the whole evaluation, so your function can read every answer without checks. The request is answered by the evaluation error handler, which responds 503 by default:

```go
semgate.WithEvaluationErrorHandler(func(w http.ResponseWriter, r *http.Request, err error, next http.Handler) {
    var apiErr *typesafe.APIError
    if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
        // invalid API key
    }
    next.ServeHTTP(w, r) // let the request through
})
```

## What is sent

Each evaluation sends a JSON state with `method`, `path`, `query`, `headers`, `body` and `body_status`. The body is sent as a UTF-8 string only when its content type matches `WithBodyContentTypes` (default: `application/json`, `application/x-www-form-urlencoded`, `application/xml`, `text/*`, `+json`, `+xml`). `body_status` is one of `none`, `complete`, `truncated`, `partial` and `omitted`.

The middleware reads only the beginning of the body and the next handler still receives the whole body as a stream.

| Option | Default | Meaning |
|---|---|---|
| `WithMaxBodyBytes(n)` | 16 KiB | Body bytes sent for evaluation (1 to `math.MaxInt-1`) |
| `WithOversizeBody(action)` | `OversizeActionTruncate` | What happens when the body exceeds the limit (below) |
| `WithOversizeRejectHandler(h)` | 413 | Response for `OversizeActionReject` |
| `WithBodyContentTypes(patterns...)` | see above | Content types whose body is sent; no argument disables bodies |
| `WithBodyReadTimeout(d)` | 1s | How long to wait for a body; after that, the bytes received so far are sent as `partial` |
| `WithEvaluateTimeout(d)` | 10s | Time limit of one API call |
| `WithEvaluationErrorHandler(fn)` | 503 | Response when the evaluation fails |
| `WithHeaderAllowlist` / `WithHeaderDenylist` | send all | Header names to send / not send (not both) |
| `WithQueryAllowlist` / `WithQueryDenylist` | send all | Query parameter names to send / not send, case-insensitive (not both) |

`OversizeAction`:

- `OversizeActionTruncate`: send the first bytes up to the limit
- `OversizeActionOmit`: evaluate without the body
- `OversizeActionSkip`: do not evaluate; call the next handler
- `OversizeActionReject`: respond 413 without evaluating

Invalid arguments to `semgate.Noul` / `Choice` / `Score`, the `Gate` methods and `g.Ask` (empty instructions, 2–255 Choice options, 2–10 Score levels, nil functions) panic when the middleware is created. `semgate.New` returns an error for invalid options.

## Providers

`semgate/providers` defines the `providers.Client` interface and its request/response types, modeled on the TypeSafe API. Implement it to use another provider or a fake in tests; `semgate/providers/typesafe` is the TypeSafe implementation.

## Development

```
go test -race ./...
golangci-lint run ./...
gosec ./...
```

`TestEvaluateLive` in `providers/typesafe` and `TestGuardsLive` in `examples/security` call the real API and run only when `TEST_TYPESAFE_API_KEY` is set. `TestGuardsLive` requires each attack payload to be answered 403 and each benign request to reach the handler.

GitHub Actions runs the same checks on every push (`test`, `lint`), plus `gosec` and `trivy`, whose findings appear in the repository's Security tab.

## License

Apache License 2.0

# examples/security

An HTTP server with one guard in front of `POST /chat`. The guard asks the model
whether the request is an injection attack and answers 403 when it probably is.

```go
func blockInjection(g *semgate.Gate) func(http.Handler) http.Handler {
	return g.Noul("Does this request contain SQL, shell, script, or path traversal injection?",
		func(w http.ResponseWriter, r *http.Request, a semgate.NoulAnswer, next http.Handler) {
			if a.Probability > 0.8 {
				http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
}
```

That is the whole library: a question about the request, the typed answer, and
your decision. `a.Probability` is the model's probability that the answer is
"yes". Change the question to detect something else; the code stays the same.
`0.8` is a tuning knob, not a recommended value.

```
TYPESAFE_API_KEY=... go run ./examples/security   # 127.0.0.1:8080
```

```sh
# 200
curl -sS -i -X POST localhost:8080/chat -H 'Content-Type: application/json' \
  -d '{"message":"What is your refund policy?"}'
# 403
curl -sS -i -X POST localhost:8080/chat -H 'Content-Type: application/json' \
  -d '{"message":"apple'"'"' OR 1=1; DROP TABLE users--"}'
```

## What the guard needs to see the payload

Each default in `newGate` leaves a way to keep the payload out of the
evaluation, so `main.go` sets all of them:

| Setting | Without it |
|---|---|
| `requireMediaType` before the guard | a body whose content type is outside `WithBodyContentTypes` is not sent, so `application/octet-stream` hides it |
| `WithOversizeBody(OversizeActionReject)` | `Truncate`, the default, evaluates the first `maxBodyBytes` and passes the whole body on; `Skip` drops the evaluation |
| `bodyReadTimeout` > `readTimeout` | the guard evaluates the part of the body that arrived and the handler gets the rest, so a payload can follow a harmless prefix |
| `WithEvaluationErrorHandler(failClosed(...))` | this is the 503 default; an error handler that calls `next` stops guarding when the provider is unreachable |
| `WithHeaderAllowlist` / `WithQueryDenylist` | every header and query parameter is sent to the provider, including `Authorization` and `Cookie` |

The header list is an allowlist and the query list a denylist, which point in
opposite directions: the allowlist keeps a header this server does not know about
from leaking, the denylist keeps a query parameter it does not know about
visible to the model. Neither reaches into the body.

The handler behind the guard runs no query, no command and no template. A guard
is one layer; it still needs parameterized queries, output escaping and path
allowlists behind it.

## Tests

```sh
go test ./examples/security/                                       # fake provider
TEST_TYPESAFE_API_KEY=... go test -run Live ./examples/security/   # real API
```

`main_test.go` holds the payloads in `attacks` and `benign`. Without a key the
provider is replaced by `fakeProvider` and the tests check the decision at and
around `0.8`, the 503, 413 and 415 responses, a body arriving in parts being
evaluated whole, `/healthz` costing no provider call, and the filtered
credentials not reaching the provider. `TestGuardLive` sends the same payloads
to the real API and requires 403 for each attack and 200 for each benign
request.

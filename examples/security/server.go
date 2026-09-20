package main

import (
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
)

// bodyMediaType is the only content type the POST routes accept. It must be one
// of the types passed to WithBodyContentTypes, otherwise the body is not sent
// for evaluation, see requireMediaType.
const bodyMediaType = "application/json"

// maxBodyBytes is how much of the body is sent for evaluation. A body larger
// than this is rejected rather than evaluated in part, see newGate.
const maxBodyBytes = 64 << 10

const (
	// readTimeout limits reading one request including its body, and is set on
	// the http.Server in main.
	readTimeout = 30 * time.Second

	// bodyReadTimeout must outlast readTimeout. When it passes first, the guard
	// evaluates the part of the body that has arrived and the next handler still
	// receives the rest: a sender could then trickle a harmless prefix, have it
	// evaluated, and deliver the payload afterwards. Outlasting readTimeout
	// means the connection's read deadline ends such a request instead.
	bodyReadTimeout = readTimeout + 5*time.Second

	// writeTimeout is reset when the request headers are read, so it covers
	// reading the body, the provider call (WithEvaluateTimeout, 10 seconds by
	// default) and the handler. A value below readTimeout would end a request
	// that the guard was still evaluating.
	writeTimeout = readTimeout + 30*time.Second
)

// evaluatedHeaders are the headers sent for evaluation. It is an allowlist:
// a header this server does not know about is not sent, so a credential in a
// header added later cannot leak through the gate. The cost is that attack
// content in an unlisted header is not evaluated either.
var evaluatedHeaders = []string{"Content-Type", "Accept", "User-Agent", "Referer"}

// credentialParams are the query parameters not sent for evaluation. Query
// parameters use a denylist and not an allowlist, the opposite of the headers
// above: the attack content of /search and /files arrives in the query, and an
// allowlist would hide a parameter this server does not know about from the
// model.
var credentialParams = []string{"token", "api_key", "access_token"}

// newGate applies the options the guards depend on: the named credentials are
// not sent, the body is read long enough to be evaluated whole, a body too
// large to evaluate is rejected instead of passed on, and a failed evaluation
// blocks the request.
func newGate(client providers.Client, logger *slog.Logger) (*semgate.Gate, error) {
	return semgate.New(client,
		// Without these options every header and query parameter is sent to the
		// provider, including Authorization and Cookie.
		semgate.WithHeaderAllowlist(evaluatedHeaders...),
		semgate.WithQueryDenylist(credentialParams...),
		// Only the content type the POST routes accept, so that the set of
		// bodies that are evaluated matches the set of bodies that are served.
		semgate.WithBodyContentTypes(bodyMediaType),
		semgate.WithMaxBodyBytes(maxBodyBytes),
		semgate.WithBodyReadTimeout(bodyReadTimeout),
		// The default truncates an oversize body, which lets a sender push the
		// payload past the evaluated prefix. OversizeActionSkip would skip the
		// evaluation altogether.
		semgate.WithOversizeBody(semgate.OversizeActionReject),
		semgate.WithEvaluationErrorHandler(failClosed(logger)),
	)
}

// failClosed answers 503 when the evaluation itself fails. An error handler
// that calls next instead stops guarding whenever the provider is unreachable,
// its key expires, or the call times out.
func failClosed(logger *slog.Logger) func(w http.ResponseWriter, r *http.Request, err error, next http.Handler) {
	return func(w http.ResponseWriter, r *http.Request, err error, _ http.Handler) {
		logger.Error("evaluation failed, request not served",
			slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	}
}

// requireMediaType answers 415 unless the body is bodyMediaType. It wraps the
// guard rather than the handler: semgate sends the body for evaluation only
// when its content type matches WithBodyContentTypes, so a route that accepts
// any content type lets a sender hide the payload from the model behind, for
// example, application/octet-stream.
func requireMediaType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != bodyMediaType {
			http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// routes mounts one guard per route. /healthz is outside every guard: a route
// behind a middleware calls the provider on every request.
func routes(gs *guards) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", accepted("healthz"))

	mux.Handle("POST /chat", requireMediaType(gs.promptInjection()(accepted("chat"))))
	mux.Handle("GET /search", gs.sqlInjection()(accepted("search")))
	mux.Handle("POST /convert", requireMediaType(gs.commandInjection()(accepted("convert"))))
	mux.Handle("POST /render", requireMediaType(gs.crossSiteScripting()(accepted("render"))))
	mux.Handle("GET /files", gs.pathTraversal()(accepted("files")))
	mux.Handle("POST /fetch", requireMediaType(gs.requestForgery()(accepted("fetch"))))

	// One provider call answers all six questions for this route.
	mux.Handle("POST /ingest", requireMediaType(gs.all()(accepted("ingest"))))
	return mux
}

// accepted answers a request that passed its guard. The example deliberately
// runs no query, no command and no template: a guard is one layer, and the
// handler behind it still needs parameterized queries, an argument vector
// instead of a shell, contextual output escaping, and path and URL allowlists.
func accepted(route string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"route": route, "status": "accepted"})
	})
}

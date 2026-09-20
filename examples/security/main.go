// Command security serves an HTTP API behind one semgate guard. The guard asks
// a System One model whether the request is an injection attack and answers 403
// when it probably is. The guard is blockInjection below.
//
//	TYPESAFE_API_KEY=... go run ./examples/security
//
//	# 200
//	curl -sS -i -X POST localhost:8080/chat -H 'Content-Type: application/json' \
//	  -d '{"message":"What is your refund policy?"}'
//	# 403
//	curl -sS -i -X POST localhost:8080/chat -H 'Content-Type: application/json' \
//	  -d '{"message":"'"'"'; DROP TABLE users--"}'
package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
	"github.com/m-mizutani/semgate/providers/typesafe"
)

// blockInjection is the whole library in one call: a question about the
// request, the typed answer, and your decision. a.Probability is the model's
// probability that the answer is "yes". 0.8 is a tuning knob, not a
// recommended value.
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

const (
	addr = "127.0.0.1:8080"

	// bodyMediaType is the only content type /chat accepts. A body whose type is
	// not in WithBodyContentTypes is not sent for evaluation.
	bodyMediaType = "application/json"

	// maxBodyBytes is how much of the body is evaluated.
	maxBodyBytes = 64 << 10

	// readTimeout limits reading one request including its body.
	readTimeout = 30 * time.Second

	// bodyReadTimeout must outlast readTimeout. When it passes first, the guard
	// evaluates the part of the body that arrived and the handler still gets the
	// rest, so a sender can deliver the payload after the evaluation.
	bodyReadTimeout = readTimeout + 5*time.Second

	// writeTimeout is reset when the request headers are read, so it must cover
	// the body read plus the provider call.
	writeTimeout = readTimeout + 30*time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	apiKey, ok := os.LookupEnv("TYPESAFE_API_KEY")
	if !ok {
		logger.Error("TYPESAFE_API_KEY is not set")
		os.Exit(1)
	}
	client, err := typesafe.New(apiKey)
	if err != nil {
		logger.Error("failed to create the provider", slog.Any("error", err))
		os.Exit(1)
	}
	gate, err := newGate(client, logger)
	if err != nil {
		logger.Error("failed to create the gate", slog.Any("error", err))
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(gate),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}
	logger.Info("listening", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

// newGate sets the options the guard depends on.
func newGate(client providers.Client, logger *slog.Logger) (*semgate.Gate, error) {
	return semgate.New(client,
		// Without these, every header and query parameter is sent to the
		// provider, including Authorization and Cookie.
		semgate.WithHeaderAllowlist("Content-Type", "Accept", "User-Agent", "Referer"),
		semgate.WithQueryDenylist("token", "api_key", "access_token"),
		semgate.WithBodyContentTypes(bodyMediaType),
		semgate.WithMaxBodyBytes(maxBodyBytes),
		semgate.WithBodyReadTimeout(bodyReadTimeout),
		// The default, OversizeActionTruncate, lets a sender push the payload
		// past the evaluated prefix; OversizeActionSkip drops the evaluation.
		semgate.WithOversizeBody(semgate.OversizeActionReject),
		semgate.WithEvaluationErrorHandler(failClosed(logger)),
	)
}

// failClosed answers 503 when the evaluation fails. Calling next instead would
// stop guarding whenever the provider is unreachable.
func failClosed(logger *slog.Logger) func(w http.ResponseWriter, r *http.Request, err error, next http.Handler) {
	return func(w http.ResponseWriter, r *http.Request, err error, _ http.Handler) {
		logger.Error("evaluation failed, request not served",
			slog.String("path", r.URL.Path), slog.Any("error", err))
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
	}
}

// requireMediaType answers 415 for a body that would not be evaluated. It wraps
// the guard, so the bodies served are the bodies evaluated.
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

// routes guards /chat only. A route behind the middleware calls the provider on
// every request.
func routes(g *semgate.Gate) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", accepted("healthz"))
	mux.Handle("POST /chat", requireMediaType(blockInjection(g)(accepted("chat"))))
	return mux
}

// accepted answers a request that passed the guard. It runs no query, no
// command and no template: the guard is one layer, and the handler behind it
// still needs parameterized queries, output escaping and path allowlists.
func accepted(route string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"route": route, "status": "accepted"})
	})
}

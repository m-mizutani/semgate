// Command security serves an HTTP API behind one semgate guard: it asks a
// System One model whether the request is an injection attack and answers 403
// when it probably is.
//
//	TYPESAFE_API_KEY=... go run ./examples/security
//
//	# 200
//	curl -sS -i -X POST localhost:8080/chat -d '{"message":"What is your refund policy?"}'
//	# 403
//	curl -sS -i -X POST localhost:8080/chat -d '{"message":"apple'"'"' OR 1=1; DROP TABLE users--"}'
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
	"github.com/m-mizutani/semgate/providers/typesafe"
)

// newServer builds the whole example: the gate, the question asked about every
// request, the typed answer, the decision, and the route the guard sits on.
func newServer(client providers.Client) *http.Server {
	mux := http.NewServeMux()

	// semgate.New holds the provider and the settings, and gate.Noul returns a
	// func(http.Handler) http.Handler, so it wraps the handler right here.
	// a.Probability is the model's probability that the answer to the question
	// is "yes"; 0.8 is a tuning knob. When the API call fails, semgate answers
	// 503 and the handler is never reached.
	gate, _ := semgate.New(client, semgate.WithHeaderDenylist("Authorization", "Cookie"))
	mux.Handle("POST /chat", gate.Noul(
		"Does this request contain SQL, shell, script, or path traversal injection?",
		func(w http.ResponseWriter, r *http.Request, a semgate.NoulAnswer, next http.Handler) {
			if a.Probability > 0.8 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		},
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})))

	return &http.Server{Addr: "127.0.0.1:8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

func main() {
	client, err := typesafe.New(os.Getenv("TYPESAFE_API_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	srv := newServer(client)
	log.Println("listening on", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

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
	"github.com/m-mizutani/semgate/providers/typesafe"
)

// chatHandler answers a request that the guard let through.
var chatHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprintln(w, "ok")
})

// newServer builds the whole example: the provider, the gate, the question
// asked about every request, the typed answer, the decision, and the route the
// guard sits on.
func newServer() *http.Server {
	mux := http.NewServeMux()

	// gate.Noul returns a func(http.Handler) http.Handler, so it wraps chatHandler
	// right here. a.Probability is the model's probability that the answer to
	// the question is "yes"; 0.8 is a tuning knob. When the API call fails,
	// semgate answers 503 and chatHandler is never reached.
	client, _ := typesafe.New(os.Getenv("TYPESAFE_API_KEY"))
	gate, _ := semgate.New(client)
	mux.Handle("POST /chat", gate.Noul(
		"Does this request contain SQL, shell, script, or path traversal injection?",
		func(w http.ResponseWriter, r *http.Request, a semgate.NoulAnswer, next http.Handler) {
			if a.Probability > 0.8 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		},
	)(chatHandler))

	return &http.Server{Addr: "127.0.0.1:8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

func main() {
	srv := newServer()
	log.Println("listening on", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

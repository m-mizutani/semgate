package semgate

import (
	"net/http"
	"slices"
	"strings"

	"github.com/m-mizutani/semgate/providers"
)

// buildState collects the request fields sent to the provider and peeks the
// body. The returned bodyResult.replay must replace the request body
// downstream.
func (g *Gate) buildState(r *http.Request) (providers.State, bodyResult) {
	body := peekBody(r, &g.cfg)
	state := providers.State{
		Method:     r.Method,
		Path:       r.URL.Path,
		Headers:    filterFields(r.Header, g.cfg.headerPolicy, http.CanonicalHeaderKey),
		Body:       string(body.snapshot),
		BodyStatus: body.status,
	}
	if query := filterFields(r.URL.Query(), g.cfg.queryPolicy, strings.ToLower); len(query) > 0 {
		state.Query = query
	}
	return state, body
}

// filterFields returns a new map so that a provider modifying it cannot
// change the headers seen by the next handler.
func filterFields(fields map[string][]string, p fieldPolicy, normalize func(string) string) map[string][]string {
	out := make(map[string][]string, len(fields))
	for name, values := range fields {
		if slices.Contains(p.names, normalize(name)) != p.allow {
			continue
		}
		out[name] = slices.Clone(values)
	}
	return out
}

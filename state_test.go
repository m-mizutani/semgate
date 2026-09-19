package semgate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate"
	"github.com/m-mizutani/semgate/providers"
)

func TestStateFields(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": noulJSON(0.1)})}
	mw := newGate(t, p).Noul("q", passNoul)

	r := httptest.NewRequest(http.MethodPost, "/api/chat?q=hello", strings.NewReader(`{"m":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	serve(mw(&downstream{}), r)

	state := p.request(t, 0).State
	gt.String(t, state.Method).Equal(http.MethodPost)
	gt.String(t, state.Path).Equal("/api/chat")
	gt.Map(t, state.Query).EqualAt("q", []string{"hello"})
	gt.Map(t, state.Headers).EqualAt("Content-Type", []string{"application/json"})
	gt.String(t, state.Body).Equal(`{"m":"hi"}`)
	gt.String(t, state.BodyStatus).Equal("complete")
}

func TestStateFieldFilters(t *testing.T) {
	run := func(t *testing.T, opts ...semgate.Option) providers.State {
		p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": noulJSON(0)})}
		r := httptest.NewRequest(http.MethodGet, "/?TOKEN=a&x=1", nil)
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("Cookie", "session=1")
		r.Header.Set("X-Foo", "foo")
		serve(newGate(t, p, opts...).Noul("q", passNoul)(&downstream{}), r)
		return p.request(t, 0).State
	}

	t.Run("no filter sends everything", func(t *testing.T) {
		state := run(t)
		gt.Map(t, state.Headers).HasKey("Authorization").HasKey("Cookie").HasKey("X-Foo")
		gt.Map(t, state.Query).HasKey("TOKEN").HasKey("x")
	})

	t.Run("header allowlist", func(t *testing.T) {
		gt.Map(t, run(t, semgate.WithHeaderAllowlist("x-foo")).Headers).Length(1).HasKey("X-Foo")
	})

	t.Run("header denylist", func(t *testing.T) {
		state := run(t, semgate.WithHeaderDenylist("Authorization", "cookie"))
		gt.Map(t, state.Headers).NotHasKey("Authorization").NotHasKey("Cookie").HasKey("X-Foo")
	})

	t.Run("query denylist ignores case", func(t *testing.T) {
		gt.Map(t, run(t, semgate.WithQueryDenylist("token")).Query).Length(1).HasKey("x")
	})

	t.Run("query allowlist", func(t *testing.T) {
		gt.Map(t, run(t, semgate.WithQueryAllowlist("X")).Query).Length(1).HasKey("x")
	})
}

func TestStateProviderCannotChangeHeaders(t *testing.T) {
	p := &fakeProvider{respond: func(ctx context.Context, req *providers.Request) (*providers.Response, error) {
		for _, values := range req.State.Headers {
			values[0] = "tampered"
		}
		return answerByInstructions(map[string]string{"q": noulJSON(0)})(ctx, req)
	}}
	d := &downstream{}
	r := getRequest()
	r.Header.Set("X-Foo", "foo")
	serve(newGate(t, p).Noul("q", passNoul)(d), r)
	gt.String(t, d.header.Get("X-Foo")).Equal("foo")
}

package semgate_test

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/m-mizutani/gt"
	"github.com/m-mizutani/semgate"
)

func TestOptionValidation(t *testing.T) {
	invalid := map[string][]semgate.Option{
		"zero max body bytes":           {semgate.WithMaxBodyBytes(0)},
		"max body bytes overflows int":  {semgate.WithMaxBodyBytes(math.MaxInt64)},
		"zero body read timeout":        {semgate.WithBodyReadTimeout(0)},
		"negative evaluate timeout":     {semgate.WithEvaluateTimeout(-1)},
		"nil evaluation error handler":  {semgate.WithEvaluationErrorHandler(nil)},
		"nil oversize reject handler":   {semgate.WithOversizeRejectHandler(nil)},
		"zero oversize action":          {semgate.WithOversizeBody(semgate.OversizeAction(0))},
		"unknown oversize action":       {semgate.WithOversizeBody(semgate.OversizeAction(99))},
		"content type without slash":    {semgate.WithBodyContentTypes("json")},
		"nil option":                    {nil},
		"header allowlist and denylist": {semgate.WithHeaderAllowlist("A"), semgate.WithHeaderDenylist("B")},
		"query allowlist and denylist":  {semgate.WithQueryAllowlist("a"), semgate.WithQueryDenylist("b")},
	}
	for name, opts := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := semgate.New(&fakeProvider{}, opts...)
			gt.Error(t, err)
		})
	}

	t.Run("valid options", func(t *testing.T) {
		_, err := semgate.New(&fakeProvider{},
			semgate.WithMaxBodyBytes(1),
			semgate.WithMaxBodyBytes(math.MaxInt-1),
			semgate.WithBodyReadTimeout(time.Millisecond),
			semgate.WithEvaluateTimeout(time.Millisecond),
			semgate.WithOversizeBody(semgate.OversizeActionReject),
			semgate.WithOversizeRejectHandler(http.NotFoundHandler()),
			semgate.WithBodyContentTypes("application/json", "text/*", "+json"),
			semgate.WithHeaderAllowlist("A"),
			semgate.WithQueryDenylist("b"),
		)
		gt.NoError(t, err)
	})
}

func TestOptionSlicesAreCopied(t *testing.T) {
	p := &fakeProvider{respond: answerByInstructions(map[string]string{"q": noulJSON(0)})}
	deny := []string{"X-Secret"}
	allowQuery := []string{"keep"}
	contentTypes := []string{"text/plain"}
	g := newGate(t, p,
		semgate.WithHeaderDenylist(deny...),
		semgate.WithQueryAllowlist(allowQuery...),
		semgate.WithBodyContentTypes(contentTypes...),
	)
	deny[0] = "X-Other"
	allowQuery[0] = "drop"
	contentTypes[0] = "application/json"

	r := httptest.NewRequest(http.MethodPost, "/?keep=1&drop=2", strings.NewReader("hello"))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("X-Secret", "s")
	r.Header.Set("X-Other", "o")
	serve(g.Noul("q", passNoul)(&downstream{}), r)

	state := p.request(t, 0).State
	gt.Map(t, state.Headers).NotHasKey("X-Secret").HasKey("X-Other")
	gt.Map(t, state.Query).Length(1).HasKey("keep")
	gt.String(t, state.BodyStatus).Equal("complete")
}

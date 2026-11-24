package going_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAChatRedirectLogsNoLocationOfIt: whatever answers at the chat endpoint answers the
// completion, which carries the deployment's bearer, with a redirect whose Location quotes that
// bearer and does not parse. The client refuses redirects, but Go parses a Location before it
// asks CheckRedirect, so the call fails with net/http's error, which the run's fault carries into
// the answerer's warning. That warning holds no run of the bearer and no word of the Location.
// The twin shows the stand-in's echo reaches net/http's own error. Captures
// going.chat-redirect/unparsed.
func TestAChatRedirectLogsNoLocationOfIt(t *testing.T) {
	const bearer = "kq7tn-not-a-secret-RDX-3e8c"
	echo := func() string {
		return redirectstest.Echo(t, "Authorization", func(credential string) string {
			return redirectstest.Unparsed(strings.TrimPrefix(credential, "Bearer "))
		})
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, echo()+"/openai/v1/chat/completions", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the row cannot see one")
		}
		if !strings.Contains(err.Error(), bearer) {
			t.Fatalf("net/http's own error holds no echo of the bearer, so the row cannot see a leak: %q", err)
		}
	})
	t.Run("unparsed", func(t *testing.T) {
		var logged logtest.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		client, err := going.NewHTTPClient(going.HTTPConfig{
			Place: "endpoint", Endpoint: echo() + "/openai/v1", Model: "gpt-test", Tokens: staticTokens{token: bearer},
		})
		if err != nil {
			t.Fatalf("NewHTTPClient: %v", err)
		}
		answerer, err := going.NewAnswerer(going.AnswererConfig{Model: client, TokensPerMinute: testTokensPerMinute})
		if err != nil {
			t.Fatalf("NewAnswerer: %v", err)
		}
		if err := answerer.Run(t.Context(), testReads().request("who owns the broadcast order"), (&answerStream{}).emit); err != nil {
			t.Fatalf("Run: %v", err)
		}
		warning := logged.Lines("going: the model service refused the run")
		if warning == "" {
			t.Fatalf("the answerer logged no refused run: %q", logged.String())
		}
		redacttest.RequireNoPiece(t, "going.chat-redirect/unparsed", warning, bearer)
		if strings.Contains(warning, "moved") {
			t.Errorf("the warning names the Location: %q", warning)
		}
	})
}

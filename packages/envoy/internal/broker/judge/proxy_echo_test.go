package judge

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
)

// lockedBuffer is a log the judge's goroutines may write while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAProxyEchoOfTheModelLeavesNoCredentialInTheRowOrTheLog: the judge's model endpoint, on
// anthropic-messages (the protocol BROKER_JUDGE_MODEL_PROTOCOL defaults to), is answered by a
// proxy's page echoing the request back, the X-Api-Key the judge sent included, bare on a line of
// its own. The run ends in a referral whose row — the one GET /v1/grant-log serves — and whose
// log line both carry the model failure, and neither carries any piece of that key: the page is
// named as a body that is not Anthropic's error JSON, with the key redacted. It swaps the
// default logger, so it does not run in parallel.
func TestAProxyEchoOfTheModelLeavesNoCredentialInTheRowOrTheLog(t *testing.T) {
	const token = "judge-proxyecho-google-id-token-0123456789"
	var (
		mu  sync.Mutex
		got []string
	)
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		key := r.Header.Get("X-Api-Key")
		mu.Lock()
		got = append(got, key)
		mu.Unlock()
		var page strings.Builder
		page.WriteString("<html><head><title>403 Forbidden</title></head><body>\n" + key + "\n<pre>\n")
		_ = r.Header.Write(&page)
		page.WriteString("</pre></body></html>\n")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, page.String())
	}))
	t.Cleanup(echo.Close)

	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	dispatch, machine := newDispatch(t, "doing", testSpec), newRequests()
	opts := testOptions(t, dispatch, newModel(t, verdictReply(Grant, "yes")), machine)
	opts.Model = Model{ID: testModel, Endpoint: agentloop.Endpoint{
		Kind: agentloop.AnthropicMessages, BaseURL: echo.URL,
		Token: func(context.Context) (string, error) { return token, nil },
	}}
	New(t.Context(), opts).consider(t.Context(), pending())

	mu.Lock()
	sent := append([]string(nil), got...)
	mu.Unlock()
	if len(sent) != 1 || sent[0] != token {
		t.Fatalf("the echo got X-Api-Key %q; want the judge's one call carrying its token, which the page then echoes", sent)
	}
	row := theOneRow(t, machine)
	if row["verdict"] != Deny || row["asked"] != true {
		t.Errorf("the row reads verdict %v, asked %v; want the no-verdict default, deny, after the model was asked", row["verdict"], row["asked"])
	}
	stored, _ := row["error"].(string)
	logged := logs.String()
	t.Logf("the row's error: %s", stored)
	t.Logf("the log: %s", logged)
	for _, c := range []struct{ where, text string }{{"the row's error", stored}, {"the log", logged}} {
		if piece := leaked(c.text, token); piece != "" {
			t.Errorf("%s holds %q of the judge's key: %s", c.where, piece, c.text)
		}
		if !strings.Contains(c.text, `a body that is not Anthropic's error JSON (\"`) && !strings.Contains(c.text, `a body that is not Anthropic's error JSON ("`) {
			t.Errorf("%s is %q; want the page named as a body that is not Anthropic's error JSON", c.where, c.text)
		}
	}
	if !strings.Contains(stored, "[redacted]") || !strings.Contains(stored, "403") {
		t.Errorf("the row's error is %q; want the status and the page with the key redacted", stored)
	}
	if !strings.Contains(logged, "model_err=") {
		t.Errorf("the log is %q; want the model failure as model_err", logged)
	}
}

// leaked is the first run of eight characters of token that text holds, and "" where it holds
// none: eight is the shortest piece of a token an excerpt redacts wherever it stands
// (packages/envoy/excerpt), so any piece of the credential at all is caught.
func leaked(text, token string) string {
	for i := range len(token) - 7 {
		if piece := token[i : i+8]; strings.Contains(text, piece) {
			return piece
		}
	}
	return ""
}

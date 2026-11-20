// packages/envoy/internal/broker/helper/broker_error_line_test.go
//go:build linux

package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
)

// TestABrokerRefusalPrintsOnOneLine: a refusal's code and words, from a broker at a clean address,
// print as they came where they hold no control character, and Go-quoted where they hold one - a
// line feed, a carriage return, an escape sequence - so the BrokerError reads as one line however
// the broker's answer is written. That holds for a refusal do decodes and for one built from the
// broker's OAuth answer (oauthBrokerError), which holds the code and words as they came. The code
// the helper matches against is still the code as it came.
func TestABrokerRefusalPrintsOnOneLine(t *testing.T) {
	answer := func(code, words string) *Broker {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "error": words})
		}))
		t.Cleanup(srv.Close)
		b := &Broker{URL: brokeraddr.New(srv.URL), HTTP: srv.Client()}
		t.Cleanup(b.Close)
		return b
	}
	refusal := func(t *testing.T, b *Broker) *BrokerError {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, b.URL.Raw()+"/v1/enrollments/self", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := b.do(req, []int{http.StatusOK}, nil)
		be, ok := got.(*BrokerError)
		if !ok {
			t.Fatalf("do = %v (%T), want a BrokerError", got, got)
		}
		return be
	}

	t.Run("plain words print as they came", func(t *testing.T) {
		be := refusal(t, answer("SESSION_HELD", "the session belongs to another pod"))
		if want := "broker 403 SESSION_HELD: the session belongs to another pod"; be.Error() != want {
			t.Errorf("Error = %q, want %q", be.Error(), want)
		}
	})
	for _, words := range []string{
		"refused\nagent-secrets: FORGED from the broker",
		"refused\r\nagent-secrets: FORGED",
		"refused\x1b[2Kagent-secrets: FORGED",
	} {
		t.Run("words "+strconv.Quote(words), func(t *testing.T) {
			const code = "SESSION_HELD"
			be := refusal(t, answer(code, words))
			want := "broker 403 " + code + ": " + strconv.Quote(words)
			if be.Error() != want {
				t.Errorf("Error = %q, want %q", be.Error(), want)
			}
			if strings.ContainsAny(be.Error(), "\r\n\x1b") {
				t.Errorf("Error = %q, which holds a control character", be.Error())
			}
			if !be.says("agent-secrets: FORGED") {
				t.Errorf("the words the helper reads are not the words as they came")
			}
		})
	}
	t.Run("a code with a line break", func(t *testing.T) {
		const code = "SESSION_HELD\nagent-secrets: FORGED"
		be := refusal(t, answer(code, "the session belongs to another pod"))
		want := "broker 403 " + strconv.Quote(code) + ": the session belongs to another pod"
		if be.Error() != want || !be.HasCode(code) {
			t.Errorf("Error = %q, want %q and the code as it came", be.Error(), want)
		}
	})
	t.Run("a refusal built from the broker's OAuth answer", func(t *testing.T) {
		be := newBrokerError(http.StatusBadRequest, redact.NewRemoteText("LOGIN_PENDING"), redact.NewRemoteText("pending\nagent-secrets: FORGED"), false)
		if want := "broker 400 LOGIN_PENDING: " + strconv.Quote("pending\nagent-secrets: FORGED"); be.Error() != want {
			t.Errorf("Error = %q, want %q", be.Error(), want)
		}
	})
}

// TestACredentialIDTheBrokerIssuedLogsOnOneLine: the helper's log lines name the credential_id
// of the machine login the broker issued, and slog's handlers, the text one
// agent-secrets-helper logs with (cmd/agent-secrets-helper) and the JSON one, already print a
// value that holds a control character escaped on the line, so such an id starts no line of its
// own and a plain id prints as it came. A second quoting on top would only print the quotes
// escaped. The premise is checked first: an id written into the line as it came does start one.
func TestACredentialIDTheBrokerIssuedLogsOnOneLine(t *testing.T) {
	oneLine := func(out string) bool {
		return strings.Count(out, "\n") == 1 && strings.HasSuffix(out, "\n") && !strings.ContainsAny(strings.TrimSuffix(out, "\n"), "\r\x1b")
	}
	const forged = "lcred-1\nagent-secrets-helper: FORGED machine login issued"
	if oneLine("level=ERROR credential_id=" + forged + "\n") {
		t.Fatal("the check passes an id written as it came, which starts a line of its own")
	}
	for _, id := range []string{"lcred-1", forged, "lcred-1\r\nFORGED", "lcred-1\x1b[2KFORGED"} {
		for name, handler := range map[string]func(*bytes.Buffer) slog.Handler{
			"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		} {
			var out bytes.Buffer
			b := &Broker{MachineFile: filepath.Join(t.TempDir(), "machine.json"), Log: slog.New(handler(&out))}
			b.unsaveFile(&machineCredential{id: id}, errors.New("no space left on device"))
			if !oneLine(out.String()) {
				t.Errorf("the %s log line of credential_id %q is %q, want one line", name, id, out.String())
			}
			want := map[string]string{"text": "credential_id=" + id, "json": `"credential_id":"` + id + `"`}[name]
			if id != "lcred-1" {
				want = map[string]string{"text": "credential_id=" + strconv.Quote(id)}[name]
				if name == "json" {
					encoded, _ := json.Marshal(id)
					want = `"credential_id":` + string(encoded)
				}
			}
			if !strings.Contains(out.String(), want) {
				t.Errorf("the %s log line of credential_id %q is %q, want it to hold %s", name, id, out.String(), want)
			}
		}
	}
}

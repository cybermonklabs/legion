// packages/envoy/internal/broker/helper/broker_error_line_test.go
//go:build linux

package helper

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
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
		if be.Error() != want || be.Code != code {
			t.Errorf("Error = %q and Code = %q, want %q and the code as it came", be.Error(), be.Code, want)
		}
	})
	t.Run("a refusal built from the broker's OAuth answer", func(t *testing.T) {
		be := &BrokerError{Status: http.StatusBadRequest, Code: "LOGIN_PENDING", Message: "pending\nagent-secrets: FORGED"}
		if want := "broker 400 LOGIN_PENDING: " + strconv.Quote("pending\nagent-secrets: FORGED"); be.Error() != want {
			t.Errorf("Error = %q, want %q", be.Error(), want)
		}
	})
}

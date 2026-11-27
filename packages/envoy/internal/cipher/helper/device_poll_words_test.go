// packages/envoy/internal/cipher/helper/device_poll_words_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/oauthrefusal"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
	"github.com/sjawhar/envoy/redirects"
)

// devicePollCredentialedURL is a broker token endpoint URL carrying a credential in its user
// information and in a query value, as the configured broker address can (cipheraddr.Address):
// nothing listens at port 1, so a poll of it gets no answer. tail makes net/url refuse the URL,
// as the address's join can still write one (Broker.request's reparsed address).
const (
	devicePollCredentialedURL = "https://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/v1/oauth/token?access" + "_token=" + redacttest.Token
	devicePollUnparsedURL     = "https://" + redacttest.User + ":" + redacttest.Password + "@127.0.0.1:1/v1/oauth%zz/token?access" + "_token=" + redacttest.Token
)

// TestADeviceCodePollThatGetsNoAnswerNamesTheURLItAsked: a machine login polls the broker's token
// endpoint (devicePoll) at the configured address, which can carry a credential in its user
// information and in a query value. A poll that gets no answer at all - nothing listening, or a
// joined URL net/url refuses, which is sent nowhere - names the request through redact.Unanswered:
// net/http's operation and the URL the poll asked (its scheme and path alone, its host and port
// written as redact's mark for a URL with credentials, since what url.Parse reads as the host of
// such a URL can be the tail of a password that holds an "@"), or nothing of a URL that does not
// parse, and then the failure's own transport words, the dialled host and port as the same mark,
// or its class. Neither print holds any part of the credentials, under any verb, in a log field
// or inside another value. Each case's twin shows net/http's and net/url's own errors quote them,
// so the row can see a leak. Captures helper.device-poll-unanswered/<case>.
func TestADeviceCodePollThatGetsNoAnswerNamesTheURLItAsked(t *testing.T) {
	const marked = "[a URL with credentials]"
	cases := []struct{ name, url, want string }{
		{"no-answer", devicePollCredentialedURL, `Post "https://` + marked + `/v1/oauth/token": dial tcp ` + marked + ":" + marked + ": "},
		{"unparsed", devicePollUnparsedURL, `parse "` + redact.NotPrinted + `": ` + redact.ClassNotAURL},
	}
	for _, c := range cases {
		t.Run("twin-"+c.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, c.url, nil)
			if c.name == "unparsed" {
				if err == nil || !slices.Equal(redacttest.Held(err.Error()), redacttest.Secrets) {
					t.Fatalf("net/url's own error is %v, holding %q; want one holding every credential, so the row can see a leak", err, redacttest.Held(err.Error()))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			//nolint:bodyclose // nothing listens at port 1: there is no answer to close.
			_, err = http.DefaultClient.Do(request)
			held := redacttest.Held(errorText(err))
			if err == nil || !slices.Contains(held, redacttest.User) || !slices.Contains(held, redacttest.Token) {
				t.Fatalf("net/http's own error is %v, holding %q; want the user information and the query value in it, so the row can see a leak", err, held)
			}
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &oauth2.Config{ClientID: "machine-login", Endpoint: oauth2.Endpoint{TokenURL: c.url}}
			tok, err := devicePoll(context.Background(), redirects.Refused(http.DefaultClient), cfg, &oauth2.DeviceAuthResponse{DeviceCode: "device-1"})
			if err == nil {
				t.Fatalf("the poll answered the token %v and no error; want the failure of a request that got no answer", tok)
			}
			capture := "helper.device-poll-unanswered/" + c.name
			redacttest.RequireNoPiece(t, capture, err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
			redacttest.RequireNoneFormatted(t, capture, err)
			if !strings.HasPrefix(err.Error(), c.want) {
				t.Errorf("the poll's failure prints %q, want it to begin %q", err, c.want)
			}
		})
	}
}

// errorText is err's text, and fixed words for no error, for a twin that names what it read.
func errorText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// TestADevicePollsRefusalHoldsTheRulesReadingAlone: the broker, or whoever answers at its address,
// refuses a device code poll with a description and an error_uri that quote a URL carrying a
// credential, a body of its own and a status line it wrote itself. The refusal devicePoll answers
// is x/oauth2's own token-endpoint error as the boundary rebuilt it (redact.NewRetrieveError over
// the answer, redact.OAuth2): the status with its standard reason phrase and never the broker's,
// the rule's reading of the code and the description, no error_uri and no body, and of the
// answer's headers the Retry-After alone, which the poll's wait still reads (retryAfter). The code
// is read as it came (redact.RefusalCode, oauthrefusal.Refusal), and no print of the refusal holds
// a credential.
func TestADevicePollsRefusalHoldsTheRulesReadingAlone(t *testing.T) {
	body := `{"error":"slow_down","error_description":"` + redacttest.Words + `","error_uri":"` + redacttest.Code + `"}`
	base := redacttest.StatusLineServer(t, http.StatusBadRequest, "Retry-After: 7\r\nContent-Type: application/json\r\nX-RateLimit-Reset: 99\r\n", body)
	cfg := &oauth2.Config{ClientID: "machine-login", Endpoint: oauth2.Endpoint{TokenURL: base + "/v1/oauth/token"}}
	_, err := devicePoll(context.Background(), redirects.Refused(http.DefaultClient), cfg, &oauth2.DeviceAuthResponse{DeviceCode: "device-1"})
	var answered *oauth2.RetrieveError
	if !errors.As(err, &answered) {
		t.Fatalf("the poll answered %v, want the broker's refusal", err)
	}
	redacttest.RequireNoneFormatted(t, "helper.device-poll-refusal", err)
	switch {
	case answered.ErrorCode != "slow_down":
		t.Errorf("the refusal's code is %q, want slow_down as it came", answered.ErrorCode)
	case answered.ErrorDescription != redacttest.Shown:
		t.Errorf("the refusal's description is %q, want the rule's reading %q", answered.ErrorDescription, redacttest.Shown)
	case answered.ErrorURI != "":
		t.Errorf("the refusal holds the error_uri %q, want none: the refusal carries the code and the description alone", answered.ErrorURI)
	case len(answered.Body) != 0:
		t.Errorf("the refusal holds the body %q, want none", answered.Body)
	case answered.Response.Status != redact.StatusLine(http.StatusBadRequest):
		t.Errorf("the refusal's status line is %q, want the standard %q and never the phrase the answering party wrote", answered.Response.Status, redact.StatusLine(http.StatusBadRequest))
	case len(answered.Response.Header) != 1 || answered.Response.Header.Get("Retry-After") != "7":
		t.Errorf("the refusal holds the headers %v, want the Retry-After alone", answered.Response.Header)
	case redact.RefusalCode(err) != redact.SlowDown, oauthrefusal.Refusal(err) != redact.SlowDown:
		t.Errorf("the refusal reads as %s, want slow_down as it came", redact.RefusalCode(err))
	case retryAfter(err) != 7*time.Second:
		t.Errorf("the poll would wait %s, want the answer's Retry-After of 7s", retryAfter(err))
	}
}

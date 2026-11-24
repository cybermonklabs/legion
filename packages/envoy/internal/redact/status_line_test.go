package redact

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// quotingReason is a status line whose reason phrase, which the answering party writes, quotes the
// probe's URL, credentials and all.
const quotingReason = "400 see https://" + redacttest.User + ":" + redacttest.Password + "@idp.example/help?session=" + redacttest.Token

// statusLineServer answers every request with "HTTP/1.1 <status>", a page that is no OAuth error
// answer, and header, written on the connection itself, since net/http writes only the standard
// reason phrase.
func statusLineServer(t *testing.T, status, header string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, request.Body)
				const body = "<html>gateway</html>"
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 %s\r\nContent-Type: text/html\r\n%sContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, header, len(body), body)
			}(conn)
		}
	}()
	return "http://" + listener.Addr().String()
}

// TestTheBoundaryPrintsNoStatusLineTheAnswerWrote: a token or device endpoint that answers with no
// OAuth error code, under a reason phrase that quotes a credential's URL, is an answer whose text
// x/oauth2 makes of its status line. Through OAuth2, the code exchange, the device authorization
// and a token source's refresh each print the status code's standard phrase, and none of the
// phrase the endpoint wrote. The control is the same endpoint's answer as x/oauth2 hands it out,
// which prints it. The designated assertion is redact.status-line/<call>.
func TestTheBoundaryPrintsNoStatusLineTheAnswerWrote(t *testing.T) {
	base := statusLineServer(t, quotingReason, "")
	config := &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{
		TokenURL: base + "/token", DeviceAuthURL: base + "/device", AuthStyle: oauth2.AuthStyleInParams,
	}}
	ctx := context.Background()
	_, raw := config.Exchange(ctx, "code")
	if len(redacttest.Held(fmt.Sprint(raw))) == 0 {
		t.Fatalf("the control: x/oauth2's own error %q prints none of the reason phrase, so the rows cannot fail", raw)
	}
	_, exchange := OAuth2(config.Exchange(ctx, "code"))
	_, device := OAuth2(config.DeviceAuth(ctx))
	_, refresh := OAuth2(config.TokenSource(ctx, &oauth2.Token{RefreshToken: "refresh"}).Token())
	for name, err := range map[string]error{"Exchange": exchange, "DeviceAuth": device, "TokenSource.Token": refresh} {
		t.Run(name, func(t *testing.T) {
			var re *oauth2.RetrieveError
			if !errors.As(err, &re) || re.Response == nil || re.Response.StatusCode != http.StatusBadRequest {
				t.Fatalf("OAuth2 = %#v, want an answer of status 400", err)
			}
			if re.Response.Status != "400 Bad Request" {
				t.Errorf("the answer's status is %q, want the standard 400 Bad Request", re.Response.Status)
			}
			redacttest.RequireNoneFormatted(t, "redact.status-line/"+name, err)
		})
	}
}

// TestTheBoundarysAnswerKeepsOnlyItsRetryAfter: the *oauth2.RetrieveError OAuth2's answer unwraps
// to keeps, of the answer's headers, Retry-After alone, rebuilt from the number or date it
// parses as, so encoding/json of it, which writes its whole response, prints no header the
// answering party wrote. A Retry-After that is neither is not kept. The designated assertion is
// redact.answer-headers.
func TestTheBoundarysAnswerKeepsOnlyItsRetryAfter(t *testing.T) {
	answer := func(retryAfter string) *oauth2.RetrieveError {
		return &oauth2.RetrieveError{
			Response: &http.Response{StatusCode: http.StatusTooManyRequests, Status: quotingReason, Header: http.Header{
				"Link":        {"<https://" + redacttest.User + ":" + redacttest.Password + "@idp.example/help?" + "session=" + redacttest.Token + ">; rel=help"},
				"Retry-After": {retryAfter},
			}},
			ErrorCode: "invalid_request", ErrorDescription: "slow down",
		}
	}
	for _, row := range []struct{ name, retryAfter, kept string }{
		{"seconds", "7", "7"},
		{"date", "Wed, 18 Nov 2026 12:00:00 GMT", "Wed, 18 Nov 2026 12:00:00 GMT"},
		{"past an int64", "99999999999999999999", "9223372036854775807"},
		{"neither", "7 https://" + redacttest.User + "@idp.example/", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := OAuth2(struct{}{}, answer(row.retryAfter))
			var re *oauth2.RetrieveError
			if !errors.As(err, &re) {
				t.Fatalf("OAuth2 = %T, want an answer", err)
			}
			data, err := json.Marshal(re)
			if err != nil {
				t.Fatal(err)
			}
			redacttest.RequireNone(t, "redact.answer-headers", string(data))
			if got := re.Response.Header.Get("Retry-After"); got != row.kept || len(re.Response.Header) > 1 {
				t.Errorf("the answer keeps the headers %v, want Retry-After %q alone", re.Response.Header, row.kept)
			}
		})
	}
}

// TestTheBoundarysURLErrorPrintsItsCallAsTheRuleReadsIt: an OAuth error answer under net/http's
// *url.Error, as an http.Client whose transport asks x/oauth2 for a token answers it, comes out
// of OAuth2 under a *url.Error whose operation and URL are the rule's reading: the URL is the
// request's, which a redirect the answering party named can set, query and all. The designated
// assertion is redact.url-error/<field>.
func TestTheBoundarysURLErrorPrintsItsCallAsTheRuleReadsIt(t *testing.T) {
	// The answer holds the request it came of, as every answer x/oauth2 makes does: its record.
	request := &http.Request{Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: "idp.example", Path: "/token"}}
	answer := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest, Request: request}, ErrorCode: "invalid_grant"}
	for name, wrapped := range map[string]*url.Error{
		"url": {Op: "Get", URL: "https://" + redacttest.User + ":" + redacttest.Password + "@idp.example/next?session=" + redacttest.Token, Err: answer},
		"op":  {Op: "Get https://" + redacttest.User + ":" + redacttest.Password + "@idp.example/", URL: "https://api.example/v1", Err: answer},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := OAuth2(struct{}{}, wrapped)
			var held *url.Error
			if !errors.As(err, &held) || held == wrapped {
				t.Fatalf("OAuth2 = %#v, want a *url.Error of its own", err)
			}
			redacttest.RequireNoneFormatted(t, "redact.url-error/"+name, err)
		})
	}
}

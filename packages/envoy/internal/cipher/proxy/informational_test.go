package proxy

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// TestAnInformationalAnswerCarriesNoProxyHeader: the bound host's service answers an allowed
// minted grant's write with a 103 that carries the proxy's own two headers (Cipher-Proxy-Forwarded: false,
// Cipher-Proxy-Code: GRANT_NOT_LIVE), then a final 407 with them again. The 103 reaches the command with
// neither header, and the final answer with Cipher-Proxy-Forwarded: true and no Cipher-Proxy-Code, so a
// service cannot pass for the proxy in any answer (statusWriter, ModifyResponse); the call's line
// names the final status, 407, not the 103. Control: the 103 itself reaches the command, so the
// check reads an informational answer that arrived.
func TestAnInformationalAnswerCarriesNoProxyHeader(t *testing.T) {
	r, _ := newDPoPRig(t)
	r.answerWith(func(w http.ResponseWriter, _ *http.Request, _ proxyrulestest.Request) bool {
		w.Header().Set(forwardedHeader, "false")
		w.Header().Set(codeHeader, codeGrantNotLive)
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set(forwardedHeader, "false")
		w.Header().Set(codeHeader, codeGrantNotLive)
		w.WriteHeader(http.StatusProxyAuthRequired)
		_, _ = io.WriteString(w, "a service answering as the proxy would")
		return true
	})
	type informational struct {
		code      int
		forwarded []string
		code2     []string
		link      string
	}
	var early []informational
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		early = append(early, informational{code: code, forwarded: header.Values(forwardedHeader), code2: header.Values(codeHeader), link: header.Get("Link")})
		return nil
	}}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodPost, "https://"+boundHost+mintedPath, strings.NewReader(`{"verdict":"pass"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "DPoP "+mintedPlaceholder)
	answer, err := r.client(mintedPlaceholder).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	if len(early) != 1 || early[0].code != http.StatusEarlyHints || early[0].link == "" {
		t.Fatalf("[%s] the command saw the informational answers %+v; want the service's one 103 with its Link header", r.protocol, early)
	}
	if len(early[0].forwarded) != 0 || len(early[0].code2) != 0 {
		t.Errorf("[%s] the service's 103 reached the command with Cipher-Proxy-Forwarded %q and Cipher-Proxy-Code %q; want neither", r.protocol, early[0].forwarded, early[0].code2)
	}
	if answer.StatusCode != http.StatusProxyAuthRequired || answer.Header.Get(forwardedHeader) != "true" || answer.Header.Get(codeHeader) != "" {
		t.Errorf("[%s] the final answer is %d with Cipher-Proxy-Forwarded %q and Cipher-Proxy-Code %q; want the service's 407, true and none", r.protocol, answer.StatusCode, answer.Header.Get(forwardedHeader), answer.Header.Get(codeHeader))
	}
	// The call's line is written once the answer is carried, which can be after the command has
	// its head: read it once it is there (callsAtLeast), as every row that reads a call line does.
	_, _ = io.Copy(io.Discard, answer.Body)
	calls := r.logs.callsAtLeast(t, 1)
	if status := calls[len(calls)-1].fields["status"]; status != int64(http.StatusProxyAuthRequired) {
		t.Errorf("[%s] the call's line names the status %v; want the final 407", r.protocol, status)
	}
}

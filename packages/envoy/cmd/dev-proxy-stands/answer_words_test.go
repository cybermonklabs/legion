package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestAnUpgradeAnswerPrintsTheServicesWordsAsTheRuleReadsThem: the service behind the proxy, or
// whatever answers in its place, answers the WebSocket upgrade with an Upgrade header, and its frame
// back, that quote a credential's URL and echo the bearer the request carried. The upgrade's
// result names the header and the frame as the rule reads them, with the request's bearer
// redacted (redact.Header, and redact.Field over redact.Tokens for the frame), and none of what the
// service's stand-in wrote; the frame's own words still print. Captures
// dev-proxy-stands.answer-words/upgrade.
func TestAnUpgradeAnswerPrintsTheServicesWordsAsTheRuleReadsThem(t *testing.T) {
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		conn, out, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack the connection: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(out, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket %s %s\r\nConnection: Upgrade\r\n\r\n", bearer, redacttest.Words)
		_ = out.Flush()
		line, _ := bufio.NewReader(out).ReadString('\n')
		fmt.Fprintf(out, "the service read %s; bearer %s; %s\n", strings.TrimSpace(line), bearer, redacttest.Words)
		_ = out.Flush()
	}))
	defer service.Close()
	proxyAddr, _ := connectProxy(t, service.Listener.Addr().String())
	dir := t.TempDir()
	ca := writeFile(t, dir, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: service.Certificate().Raw})))
	proxyFile := writeFile(t, dir, "proxy", fixtureProxy(proxyAddr)+"\n")
	headerFile := writeFile(t, dir, "header", redacttest.Token+"\n")
	var stdout, stderr strings.Builder
	if code := run(context.Background(), []string{"load", "upgrade", "--proxy-file", proxyFile, "--header-file", headerFile,
		"--host", "example.com", "--ca", ca}, &stdout, &stderr); code != 0 {
		t.Fatalf("dev-airlock-stands load upgrade exited %d; stderr: %s", code, stderr.String())
	}
	redacttest.RequireNone(t, "dev-proxy-stands.answer-words/upgrade", stdout.String())
	var result struct {
		Status  int
		Upgrade string
		Frames  string
	}
	if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil {
		t.Fatalf("load upgrade answered %q: %v", stdout.String(), err)
	}
	if result.Status != http.StatusSwitchingProtocols || !strings.HasPrefix(result.Upgrade, "websocket") || !strings.Contains(result.Upgrade, redacttest.Shown) ||
		!strings.Contains(result.Frames, "the service read a frame from the command") || !strings.Contains(result.Frames, redacttest.Shown) {
		t.Errorf("load upgrade answered %+v; want 101, the Upgrade header and the frame each as the rule reads them", result)
	}
}

// refusingProxy is a proxy that refuses every CONNECT with 407 and the page page writes of the
// Proxy-Authorization the CONNECT carried, and answers its address.
func refusingProxy(t *testing.T, page func(proxyAuthorization string) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				body := page(request.Header.Get("Proxy-Authorization"))
				fmt.Fprintf(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			}()
		}
	}()
	return listener.Addr().String()
}

// TestAConnectRefusalPrintsTheProxysWordsAsTheRuleReadsThem: the proxy, or whatever answers in its
// place, refuses the CONNECT with 407 and a page that echoes the Proxy-Authorization the CONNECT
// carried, the user and password it decodes from it, and words that quote a credential's URL. The
// refusal names the page's words as the rule reads them, with every credential the CONNECT carried
// redacted (redact.Carried over the CONNECT the answer is read with), and none of what the page
// echoed (echo). A page that echoes nothing prints as the proxy wrote it (control). Captures
// dev-proxy-stands.answer-words/connect.
func TestAConnectRefusalPrintsTheProxysWordsAsTheRuleReadsThem(t *testing.T) {
	refusal := func(t *testing.T, page func(proxyAuthorization string) string) (printed, reason string) {
		t.Helper()
		proxy := &url.URL{Scheme: "http", User: url.UserPassword(redacttest.User, redacttest.Password), Host: refusingProxy(t, page)}
		proxyFile := writeFile(t, t.TempDir(), "proxy", proxy.String()+"\n")
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"load", "gap", "--proxy-file", proxyFile, "--host", "example.com", "--gap", "0"}, &stdout, &stderr); code != 0 {
			t.Fatalf("dev-airlock-stands load gap exited %d; stderr: %s", code, stderr.String())
		}
		var result struct{ Error string }
		if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil {
			t.Fatalf("load gap answered %q: %v", stdout.String(), err)
		}
		return stdout.String(), result.Error
	}
	t.Run("echo", func(t *testing.T) {
		printed, reason := refusal(t, func(proxyAuthorization string) string {
			return "refused Proxy-Authorization: " + proxyAuthorization + "; user " + redacttest.User + ", password " + redacttest.Password + "; " + redacttest.Words
		})
		redacttest.RequireNone(t, "dev-proxy-stands.answer-words/connect", printed)
		if basic := base64.StdEncoding.EncodeToString([]byte(redacttest.User + ":" + redacttest.Password)); strings.Contains(printed, basic) {
			t.Errorf("load gap printed the Basic credential the CONNECT carried: %s", printed)
		}
		if !strings.HasPrefix(reason, "the CONNECT answered 407 refused Proxy-Authorization: ") || !strings.Contains(reason, redacttest.Shown) {
			t.Errorf("load gap answered the error %q; want the 407 and the page's words as the rule reads them", reason)
		}
	})
	t.Run("control", func(t *testing.T) {
		_, reason := refusal(t, func(string) string { return "proxy authentication required" })
		if want := "the CONNECT answered 407 proxy authentication required"; reason != want {
			t.Errorf("load gap answered the error %q; want %q, the page as the proxy wrote it", reason, want)
		}
	})
}

// TestAnEnrolmentRefusalPrintsTheBrokersWordsAsTheRuleReadsThem: the broker, or whatever answers in
// its place, takes the service login and refuses the enrolment with a page that echoes the launcher
// proof and the pod's token the enrolment carried, and words that quote a credential's URL. The
// refusal names the page's words as the rule reads them, with every credential the enrolment
// carried redacted (redact.Carried over the request net/http leaves on the answer), and neither
// the proof nor the token. Captures dev-proxy-stands.answer-words/enroll.
func TestAnEnrolmentRefusalPrintsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	var mu sync.Mutex
	var proofs []string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/launcher-credentials/token":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"credential_id":"credential-1"}`)
		case "/v1/enrollments":
			var body struct {
				PodToken string `json:"pod_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			proofs = append(proofs, r.Header.Get("Proof"))
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "enrolment refused: proof %s, pod token %s; %s", r.Header.Get("Proof"), body.PodToken, redacttest.Words)
		default:
			http.NotFound(w, r)
		}
	}))
	defer broker.Close()
	dir := t.TempDir()
	tokenFile := writeFile(t, dir, "registrar-token", "registrar-token-not-echoed\n")
	podTokenFile := writeFile(t, dir, "pod-token", redacttest.Token+"\n")
	var stdout, stderr strings.Builder
	if code := run(context.Background(), []string{"enroll-pod", "--url", broker.URL, "--token-file", tokenFile, "--pod-token-file", podTokenFile,
		"--pod-uid", "pod-uid-1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("dev-airlock-stands enroll-pod exited %d; want 1, the enrolment refused. stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	printed := stderr.String()
	redacttest.RequireNone(t, "dev-proxy-stands.answer-words/enroll", printed)
	mu.Lock()
	defer mu.Unlock()
	if len(proofs) != 1 || proofs[0] == "" {
		t.Fatalf("the enrolment carried the proofs %q; want one, which the page echoes", proofs)
	}
	if strings.Contains(printed, proofs[0]) {
		t.Errorf("enroll-pod printed the launcher proof the enrolment carried: %s", printed)
	}
	if want := "/v1/enrollments answered 403: enrolment refused: proof "; !strings.Contains(printed, want) || !strings.Contains(printed, redacttest.Shown) {
		t.Errorf("enroll-pod printed %q; want %q and the page's words as the rule reads them", printed, want)
	}
}

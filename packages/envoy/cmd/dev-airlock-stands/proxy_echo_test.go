// proxy_echo_test.go: a gateway in the broker's place, a proxy that refuses a CONNECT, or a
// service on a tunnel's far side can answer with a page of its own that echoes the request back,
// its headers and its body. These rows drive the enrolment's two calls, the load check's CONNECT
// and its WebSocket upgrade against such a page and read what the command prints: the page is
// named in one line, and no credential the request carried is in it. The broker's and the
// airlock's own refusals print as they always did.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTheEnrolmentNamesAProxysPageWithoutTheCredentialsItCarried: a gateway answers the service
// login, or the enrolment after it, with a page echoing the request: the registrar's token and
// the signed request object in the login's body, the launcher proof and the pod's token in the
// enrolment's, each bare on a line of its own too. The refusal on stderr names the page in one
// line and holds none of them. The broker's own error JSON prints whole, as it always did.
func TestTheEnrolmentNamesAProxysPageWithoutTheCredentialsItCarried(t *testing.T) {
	const ownRefusal = `{"code":"ACCOUNT_NOT_LISTED","error":"the token's account is not one BROKER_SERVICE_LOGINS lists"}`
	for _, row := range []struct {
		name, refusedPath, own, want string
	}{
		{"the service login", "/v1/launcher-credentials/token", "", `answered 403: a body that is not the broker's error JSON ("`},
		{"the enrolment", "/v1/enrollments", "", `answered 403: a body that is not the broker's error JSON ("`},
		{"the broker's own refusal", "/v1/launcher-credentials/token", ownRefusal, "answered 403: " + ownRefusal},
	} {
		t.Run(row.name, func(t *testing.T) {
			var mu sync.Mutex
			var carried []string
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var fields map[string]any
				_ = json.Unmarshal(body, &fields)
				var sent []string
				for _, value := range []any{r.Header.Get("Proof"), fields["token"], fields["request"], fields["pod_token"]} {
					if text, _ := value.(string); text != "" {
						sent = append(sent, text)
					}
				}
				mu.Lock()
				carried = append(carried, sent...)
				mu.Unlock()
				if r.URL.Path != row.refusedPath {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"credential_id":"cred-1"}`)
					return
				}
				if row.own != "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, row.own)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintf(w, "<html><head><title>403 Forbidden</title></head><body><h1>403 Forbidden</h1><pre>%s %s\nProof: %s\n\n%s\n%s\n</pre></body></html>",
					r.Method, r.URL.Path, r.Header.Get("Proof"), body, strings.Join(sent, "\n"))
			}))
			defer broker.Close()
			dir := t.TempDir()
			tokenFile, podTokenFile := filepath.Join(dir, "registrar"), filepath.Join(dir, "pod")
			for path, token := range map[string]string{tokenFile: "eyJregistrarN4wQ8tK1zX6cV3", podTokenFile: "eyJpodtokenB2mR7yH5sL9dF4"} {
				if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr bytes.Buffer
			code := runEnrollPod(context.Background(), []string{"--url", broker.URL, "--token-file", tokenFile,
				"--pod-token-file", podTokenFile, "--pod-uid", "pod-uid-1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr)
			mu.Lock()
			sent := append([]string(nil), carried...)
			mu.Unlock()
			if len(sent) < 2 {
				t.Fatalf("the broker saw %d credentials, want each call's", len(sent))
			}
			for _, credential := range sent {
				if strings.Contains(stderr.String(), credential) {
					t.Errorf("the refusal prints the credential %q: %s", credential, stderr.String())
				}
			}
			if code != 1 || !strings.Contains(stderr.String(), row.want) {
				t.Errorf("enroll-pod = exit %d, stderr %q; want exit 1 and stderr holding %q", code, stderr.String(), row.want)
			}
			t.Logf("enroll-pod's stderr: %s", stderr.String())
		})
	}
}

// proxySecret is the secret every load row's proxy URL carries as its user info, and
// proxyCredential the Proxy-Authorization a CONNECT sends with it.
const proxySecret = "proxyC7xN2vB8x"

var proxyCredential = base64.StdEncoding.EncodeToString([]byte("agent-secrets:" + proxySecret))

// TestTheCONNECTNamesAProxysPageWithoutItsPassword: a proxy refuses the CONNECT 407 with a page
// echoing the Proxy-Authorization it was sent, the credential bare on a line of its own too. The
// error names the page in one line, without the credential or the password in it. The airlock's
// own refusal, its "airlock: " text, prints as it always did.
func TestTheCONNECTNamesAProxysPageWithoutItsPassword(t *testing.T) {
	const ownRefusal = "airlock: the proxy password is not one the broker issued (answer 01JAIRLOCKANSWER)"
	for _, row := range []struct {
		name, own, want string
	}{
		{"a proxy's page", "", `the CONNECT answered 407 with a body that is not the airlock's refusal ("`},
		{"the airlock's own refusal", ownRefusal, "the CONNECT answered 407 " + ownRefusal},
	} {
		t.Run(row.name, func(t *testing.T) {
			var sent string
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent = r.Header.Get("Proxy-Authorization")
				if row.own != "" {
					http.Error(w, row.own, http.StatusProxyAuthRequired)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusProxyAuthRequired)
				_, _ = fmt.Fprintf(w, "<html><body><h1>407</h1><pre>%s %s\nProxy-Authorization: %s\n%s\n%s\n</pre></body></html>",
					r.Method, r.Host, sent, proxyCredential, proxySecret)
			}))
			defer proxy.Close()
			o := &loadOptions{proxyURL: "http://agent-secrets:" + proxySecret + "@" + proxy.Listener.Addr().String(), host: "s.airlock.test"}
			conn, err := o.connect(10 * time.Second)
			if conn != nil {
				conn.Close()
			}
			if sent != "Basic "+proxyCredential {
				t.Fatalf("the CONNECT carried Proxy-Authorization %q, want the proxy URL's", sent)
			}
			if err == nil {
				t.Fatal("the CONNECT answered no error")
			}
			for _, credential := range []string{proxyCredential, proxySecret} {
				if strings.Contains(err.Error(), credential) {
					t.Errorf("the error prints %q: %v", credential, err)
				}
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Errorf("the error = %v, want it to hold %q", err, row.want)
			}
			t.Logf("the CONNECT answered: %v", err)
		})
	}
}

// tunnelTo is a CONNECT proxy that takes every CONNECT and joins it to upstream, whatever host
// it names, as the airlock does once the password is right; it stops when the row ends.
func tunnelTo(t *testing.T, upstream string) string {
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
				reader := bufio.NewReader(conn)
				if _, err := http.ReadRequest(reader); err != nil {
					return
				}
				far, err := net.Dial("tcp", upstream)
				if err != nil {
					return
				}
				defer far.Close()
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
					return
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(far, reader); done <- struct{}{} }()
				go func() { _, _ = io.Copy(conn, far); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return listener.Addr().String()
}

// TestTheUpgradeNamesAServicesAnswerWithoutTheBearer: on the tunnel's far side, the upgrade is
// answered either with a page echoing the request, its bearer bare on a line of its own too, or
// with a 101 whose first line back echoes it. The result the load check prints names the page in
// one line, and the line through the same redaction, without the bearer.
func TestTheUpgradeNamesAServicesAnswerWithoutTheBearer(t *testing.T) {
	const bearer = "PLACEHOLDERbearerM3xQ8vR2kT6"
	for _, row := range []struct {
		name, field, want string
		switched          bool
	}{
		{"a page in place of the upgrade", "body", `a body that is not the airlock's refusal ("`, false},
		{"a line after the 101", "frames", "the service read a frame from the command", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			var sent string
			service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent = r.Header.Get("Authorization")
				if !row.switched {
					w.Header().Set("Content-Type", "text/html")
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprintf(w, "<html><body><pre>Authorization: %s\n%s\n</pre></body></html>", sent, bearer)
					return
				}
				conn, buffered, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				_ = buffered.Flush()
				frame, _ := buffered.ReadString('\n')
				_, _ = buffered.WriteString("the service read " + strings.TrimSpace(frame) + " under " + sent + " " + bearer + "\n")
				_ = buffered.Flush()
			}))
			defer service.Close()
			pool := x509.NewCertPool()
			pool.AddCert(service.Certificate())
			o := &loadOptions{proxyURL: "http://agent-secrets:" + proxySecret + "@" + tunnelTo(t, service.Listener.Addr().String()),
				host: "example.com", header: bearer, pool: pool}

			var stdout bytes.Buffer
			o.upgrade(&stdout)
			if sent != "Bearer "+bearer {
				t.Fatalf("the upgrade carried Authorization %q, want the bearer; the command printed %s", sent, stdout.String())
			}
			for _, credential := range []string{bearer, proxyCredential, proxySecret} {
				if strings.Contains(stdout.String(), credential) {
					t.Errorf("the result prints %q: %s", credential, stdout.String())
				}
			}
			var result map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("the result %q is not JSON: %v", stdout.String(), err)
			}
			if got, _ := result[row.field].(string); !strings.Contains(got, row.want) {
				t.Errorf("the result's %s = %q, want it to hold %q", row.field, got, row.want)
			}
			t.Logf("the result: %s", stdout.String())
		})
	}
}

// TestTheEnrolmentNamesItsURLWithoutTheUserInfoItCarries: --url can carry a user and a password,
// which net/http sends as Basic authorization, so neither a refusal nor a failed call names that
// URL whole: each names it as internal/redact prints a URL a person configured.
func TestTheEnrolmentNamesItsURLWithoutTheUserInfoItCarries(t *testing.T) {
	const userSecret = "pw-Q7vK2mXr9LtW"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"ACCOUNT_NOT_LISTED","error":"not listed"}`)
	}))
	defer server.Close()
	refused := call(context.Background(), http.MethodPost, userInfoURL(strings.TrimPrefix(server.URL, "http://"), "/v1/launcher-credentials/token", userSecret),
		nil, map[string]string{}, &struct{}{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	unreached := call(context.Background(), http.MethodPost, userInfoURL(closed, "/v1/enrollments", userSecret),
		nil, map[string]string{}, &struct{}{})
	for name, err := range map[string]error{"refusal": refused, "failed call": unreached} {
		if err == nil || strings.Contains(err.Error(), userSecret) {
			t.Errorf("the %s = %v, want a failure that does not print the URL's password", name, err)
		}
	}
	if refused == nil || !strings.Contains(refused.Error(), `answered 403: {"code":"ACCOUNT_NOT_LISTED"`) {
		t.Errorf("the refusal = %v, want the broker's own refusal named", refused)
	}
}

// userInfoURL is http://host/path with secret as the password of its user info, as --url can
// carry one.
func userInfoURL(host, path, secret string) string {
	return (&url.URL{Scheme: "http", User: url.UserPassword("registrar", secret), Host: host, Path: path}).String()
}

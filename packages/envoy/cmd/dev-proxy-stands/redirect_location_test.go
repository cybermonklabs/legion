package main

import (
	"bufio"
	"context"
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
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// TestAnEnrolmentRedirectPrintsNoLocationOfIt: whatever answers at the broker's address answers a
// call of enroll-pod with a redirect whose Location, which net/http cannot parse, quotes what the
// call carried: the registrar's token in the service login's body (login), or the launcher proof
// and the pod's token of the enrolment (enrolment). Go parses a Location before it asks
// CheckRedirect, so net/http's error quotes it although the client follows no redirect. What
// enroll-pod prints holds no run of any of them and no word of the Location. The twin shows the
// stand-in's echo reaches net/http's own error. Captures dev-proxy-stands.enroll-redirect/<case>.
func TestAnEnrolmentRedirectPrintsNoLocationOfIt(t *testing.T) {
	const registrarToken = "not-a-secret-registrar-RDX-41c7"
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, redirectstest.Echo(t, "Proof", redirectstest.Unparsed)+"/v1/enrollments", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Proof", registrarToken)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), registrarToken) {
			t.Fatalf("net/http's own error holds no echo of the proof, so the rows cannot see a leak: %q", err)
		}
	})
	for _, at := range []string{"login", "enrolment"} {
		t.Run(at, func(t *testing.T) {
			var mu sync.Mutex
			var carried []string
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Token    string `json:"token"`
					PodToken string `json:"pod_token"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				var echoed string
				switch {
				case r.URL.Path == "/v1/launcher-credentials/token" && at == "login":
					echoed = body.Token
				case r.URL.Path == "/v1/launcher-credentials/token":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"credential_id":"credential-1"}`)
					return
				default:
					echoed = r.Header.Get("Proof") + "." + body.PodToken
				}
				mu.Lock()
				carried = append(carried, echoed)
				mu.Unlock()
				w.Header().Set("Location", redirectstest.Unparsed(echoed))
				w.WriteHeader(http.StatusFound)
			}))
			t.Cleanup(broker.Close)
			dir := t.TempDir()
			tokenFile := writeFile(t, dir, "registrar-token", registrarToken+"\n")
			podTokenFile := writeFile(t, dir, "pod-token", redacttest.Token+"\n")
			var stdout, stderr strings.Builder
			if code := run(context.Background(), []string{"enroll-pod", "--url", broker.URL, "--token-file", tokenFile, "--pod-token-file", podTokenFile,
				"--pod-uid", "pod-uid-1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr); code != 1 {
				t.Fatalf("dev-airlock-stands enroll-pod exited %d; want 1, the call failed. stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(carried) != 1 || len(carried[0]) < 8 {
				t.Fatalf("the stand-in echoed %q; want one call's credentials", carried)
			}
			redacttest.RequireNoPiece(t, "dev-proxy-stands.enroll-redirect/"+at, stderr.String(), strings.Split(carried[0], ".")...)
			if strings.Contains(stderr.String(), "moved") {
				t.Errorf("enroll-pod printed the Location: %q", stderr.String())
			}
		})
	}
}

// TestABoundProbeRedirectPrintsNoLocationOfIt: whatever answers at the bound host answers
// probe-bound's GET, which carries AIRLOCK_DEMO_TOKEN as its bearer, with a redirect whose Location
// quotes that bearer. One net/http cannot parse (unparsed) is net/http's error whatever the
// client's redirect policy; one that parses names an address on another host nothing listens at
// (elsewhere), which a following client asks, naming the Location in its error. What probe-bound
// prints holds no run of the bearer and no word of the Location, and the stand-in a Location on
// another host names is asked nothing (followed). The twin shows the stand-in's echo reaches
// net/http's own error. Captures dev-proxy-stands.probe-redirect/<case>.
func TestABoundProbeRedirectPrintsNoLocationOfIt(t *testing.T) {
	const bearer = "not-a-secret-demo-RDX-9e02"
	dead := redirectstest.Dead(t)
	echo := func(location func(bearer string) string) string {
		return redirectstest.Echo(t, "Authorization", func(authorization string) string {
			return location(strings.TrimPrefix(authorization, "Bearer "))
		})
	}
	probe := func(t *testing.T, address string) string {
		t.Setenv("AIRLOCK_S_URL", address)
		t.Setenv("AIRLOCK_DEMO_TOKEN", bearer)
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"probe-bound"}, &stdout, &stderr); code != 0 {
			t.Fatalf("dev-airlock-stands probe-bound exited %d; stderr: %s", code, stderr.String())
		}
		return stdout.String()
	}
	t.Run("twin", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, echo(redirectstest.Unparsed)+"/go", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("net/http answered the unparsed Location with no error: the rows cannot see one")
		}
		if !strings.Contains(err.Error(), bearer) {
			t.Fatalf("net/http's own error holds no echo of the bearer, so the rows cannot see a leak: %q", err)
		}
	})
	for _, c := range []struct {
		name     string
		location func(bearer string) string
	}{
		{"unparsed", redirectstest.Unparsed},
		{"elsewhere", func(bearer string) string { return redirectstest.To(dead, bearer) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			printed := probe(t, echo(c.location))
			redacttest.RequireNoPiece(t, "dev-proxy-stands.probe-redirect/"+c.name, printed, bearer)
			if strings.Contains(printed, "moved") {
				t.Errorf("probe-bound printed the Location: %q", printed)
			}
		})
	}
	t.Run("followed", func(t *testing.T) {
		target, asked := redirectstest.Target(t)
		if printed := probe(t, echo(func(bearer string) string { return redirectstest.To(target, bearer) })); printed != "302\n" {
			t.Errorf("probe-bound printed %q; want the redirect's own status, 302", printed)
		}
		if n := asked(); n != 0 {
			t.Errorf("the stand-in the Location names was asked %d times; want none", n)
		}
	})
}

// answeringProxy is a proxy that answers every CONNECT with the status line line writes of the
// Proxy-Authorization the CONNECT carried, and answers its address.
func answeringProxy(t *testing.T, line func(proxyAuthorization string) string) string {
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
				fmt.Fprintf(conn, "%s\r\n\r\n", line(request.Header.Get("Proxy-Authorization")))
			}()
		}
	}()
	return listener.Addr().String()
}

// TestALoadAnswerPrintsNoLineItCouldNotRead: the proxy answers the CONNECT, or the service behind
// it answers a request, with a status line http.ReadResponse cannot read that quotes the credential
// the request carried: the password of the CONNECT's Proxy-Authorization (connect), or the bearer
// of the request in the tunnel (ask). http.ReadResponse's error quotes that line; the result load
// prints holds no run of the credential. The twin shows the line reaches http.ReadResponse's own
// error. Captures dev-proxy-stands.load-status-line/<case>.
func TestALoadAnswerPrintsNoLineItCouldNotRead(t *testing.T) {
	malformed := func(credential string) string { return "HTTP/1.1 " + credential + " Found" }
	t.Run("twin", func(t *testing.T) {
		sent, err := http.NewRequest(http.MethodGet, "https://example.com/first-event", nil)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := http.ReadResponse(bufio.NewReader(strings.NewReader(malformed(redacttest.Token)+"\r\n\r\n")), sent)
		if err == nil {
			_ = answer.Body.Close()
			t.Fatal("http.ReadResponse read the malformed status line: the rows cannot see an error")
		}
		if !strings.Contains(err.Error(), redacttest.Token) {
			t.Fatalf("http.ReadResponse's own error holds no echo of the bearer, so the rows cannot see a leak: %q", err)
		}
	})
	gap := func(t *testing.T, proxyURL string, extra ...string) string {
		t.Helper()
		proxyFile := writeFile(t, t.TempDir(), "proxy", proxyURL+"\n")
		var stdout, stderr strings.Builder
		args := append([]string{"load", "gap", "--proxy-file", proxyFile, "--host", "example.com", "--gap", "0"}, extra...)
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("dev-airlock-stands load gap exited %d; stderr: %s", code, stderr.String())
		}
		var result struct{ Error string }
		if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil || result.Error == "" {
			t.Fatalf("load gap answered %q (%v); want an error", stdout.String(), err)
		}
		return stdout.String()
	}
	t.Run("connect", func(t *testing.T) {
		proxy := answeringProxy(t, func(proxyAuthorization string) string {
			request := &http.Request{Header: http.Header{"Authorization": {proxyAuthorization}}}
			_, password, _ := request.BasicAuth()
			return malformed(password)
		})
		printed := gap(t, (&url.URL{Scheme: "http", User: url.UserPassword(redacttest.User, redacttest.Password), Host: proxy}).String())
		redacttest.RequireNoPiece(t, "dev-proxy-stands.load-status-line/connect", printed, redacttest.Password)
		if strings.Contains(printed, "malformed") {
			t.Errorf("load gap printed http.ReadResponse's words for the status line: %q", printed)
		}
	})
	t.Run("ask", func(t *testing.T) {
		service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, out, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack the connection: %v", err)
				return
			}
			defer conn.Close()
			fmt.Fprintf(out, "%s\r\n\r\n", malformed(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
			_ = out.Flush()
		}))
		t.Cleanup(service.Close)
		proxy, _ := connectProxy(t, service.Listener.Addr().String())
		dir := t.TempDir()
		ca := writeFile(t, dir, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: service.Certificate().Raw})))
		headerFile := writeFile(t, dir, "header", redacttest.Token+"\n")
		printed := gap(t, fixtureProxy(proxy), "--header-file", headerFile, "--ca", ca)
		redacttest.RequireNoPiece(t, "dev-proxy-stands.load-status-line/ask", printed, redacttest.Token)
		if strings.Contains(printed, "malformed") {
			t.Errorf("load gap printed http.ReadResponse's words for the status line: %q", printed)
		}
	})
}

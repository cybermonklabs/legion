// packages/envoy/cmd/dev-proxy-stands/load_test.go
// The load driver takes the proxy URL, with the grant's proxy password in it, and the bearer each
// request carries from files, and refuses both as arguments, which every process of the pod reads
// in /proc/<pid>/cmdline.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// connectProxy is a CONNECT proxy that tunnels every CONNECT to target, whatever host it named,
// and answers the Proxy-Authorization each one carried.
func connectProxy(t *testing.T, target string) (addr string, seen func() []string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var mu sync.Mutex
	var credentials []string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				request, err := http.ReadRequest(reader)
				if err != nil || request.Method != http.MethodConnect {
					return
				}
				mu.Lock()
				credentials = append(credentials, request.Header.Get("Proxy-Authorization"))
				mu.Unlock()
				upstream, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer upstream.Close()
				if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
					return
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(upstream, reader); done <- struct{}{} }()
				go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return listener.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(credentials)
	}
}

// writeFile writes text to a file of its own under dir and answers its path.
func writeFile(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The proxy password and the placeholder the tests hand the driver, which no refusal may repeat.
const (
	fixturePassword    = "proxy-password-not-a-secret"
	fixturePlaceholder = "the-placeholder"
)

// fixtureProxy is the proxy URL at addr with the grant's user and fixturePassword in it, as
// airlock-check.sh's proxy_for writes one.
func fixtureProxy(addr string) string {
	return (&url.URL{Scheme: "http", User: url.UserPassword("agent-secrets", fixturePassword), Host: addr}).String()
}

// TestTheLoadDriverReadsTheProxyAndTheHeaderFromFiles drives the gap mode through a CONNECT proxy
// to a TLS service, with the proxy URL and the bearer each in a file as the check writes them,
// newline and all: the CONNECT carries the URL's user and password as its Basic credential, and
// each request in the tunnel carries the bearer.
func TestTheLoadDriverReadsTheProxyAndTheHeaderFromFiles(t *testing.T) {
	var mu sync.Mutex
	var bearers []string
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	defer service.Close()
	proxyAddr, proxied := connectProxy(t, service.Listener.Addr().String())
	dir := t.TempDir()
	ca := writeFile(t, dir, "ca.pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: service.Certificate().Raw})))
	proxyFile := writeFile(t, dir, "proxy", fixtureProxy(proxyAddr)+"\n")
	headerFile := writeFile(t, dir, "header", fixturePlaceholder+"\n")

	var stdout, stderr strings.Builder
	// The run's context is never cancelled: runLoad ends the process when it is.
	code := run(context.Background(), []string{"load", "gap", "--proxy-file", proxyFile, "--header-file", headerFile,
		"--host", "example.com", "--ca", ca, "--gap", "0"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dev-airlock-stands load gap exited %d; stderr: %s", code, stderr.String())
	}
	var result struct{ First, Second int }
	if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil || result.First != http.StatusOK || result.Second != http.StatusOK {
		t.Fatalf("load gap answered %q (%v); want both events answered 200", stdout.String(), err)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("agent-secrets:"+fixturePassword))
	if got := proxied(); !slices.Equal(got, []string{basic}) {
		t.Errorf("the proxy was sent Proxy-Authorization %q; want one CONNECT carrying %q, the user and password of the URL in --proxy-file", got, basic)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"Bearer " + fixturePlaceholder, "Bearer " + fixturePlaceholder}; !slices.Equal(bearers, want) {
		t.Errorf("the service was sent Authorization %q; want %q, the bearer in --header-file", bearers, want)
	}
}

// TestTheLoadDriverRefusesACredentialInItsArguments: --proxy and --header, which took the proxy
// password and the placeholder as arguments, are refused by name in each spelling and point at
// the file flag, without the value in the refusal; a file that holds nothing and a CONNECT mode
// with no proxy are usage errors too.
func TestTheLoadDriverRefusesACredentialInItsArguments(t *testing.T) {
	dir := t.TempDir()
	proxyFile := writeFile(t, dir, "proxy", fixtureProxy("127.0.0.1:1")+"\n")
	empty := writeFile(t, dir, "empty", "\n")
	for name, tc := range map[string]struct {
		args []string
		says string
	}{
		"the proxy as an argument":          {[]string{"load", "gap", "--proxy", fixtureProxy("127.0.0.1:1")}, "--proxy is refused"},
		"the proxy as an argument, with =":  {[]string{"load", "load", "-proxy=" + fixtureProxy("127.0.0.1:1")}, "--proxy-file"},
		"the header as an argument":         {[]string{"load", "upgrade", "--proxy-file", proxyFile, "--header", fixturePlaceholder}, "--header is refused"},
		"the header as an argument, with =": {[]string{"load", "upload", "--proxy-file", proxyFile, "--header=" + fixturePlaceholder}, "--header-file"},
		"a proxy file that holds nothing":   {[]string{"load", "gap", "--proxy-file", empty}, "which holds nothing"},
		"a header file that holds nothing":  {[]string{"load", "gap", "--proxy-file", proxyFile, "--header-file", empty}, "which holds nothing"},
		"a CONNECT mode with no proxy":      {[]string{"load", "gap", "--host", "s.airlock.test"}, "needs the proxy in --proxy-file"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr strings.Builder
			if code := run(context.Background(), tc.args, io.Discard, &stderr); code != exitUsageError {
				t.Fatalf("dev-airlock-stands %v exited %d; want %d. stderr: %s", tc.args, code, exitUsageError, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.says) {
				t.Errorf("the refusal reads %q; want it to say %q", stderr.String(), tc.says)
			}
			for _, credential := range []string{fixturePassword, fixturePlaceholder} {
				if strings.Contains(stderr.String(), credential) {
					t.Errorf("the refusal repeats the credential %q: %s", credential, stderr.String())
				}
			}
		})
	}
}

// TestTheLoadDriverNamesAnUnreadableProxyURLWithoutItsPassword: a proxy URL the driver cannot parse
// is reported by the flag it came from, never by its text, which holds the grant's proxy password.
func TestTheLoadDriverNamesAnUnreadableProxyURLWithoutItsPassword(t *testing.T) {
	dir := t.TempDir()
	// A host whose bracket is never closed, which url.Parse refuses; the URL holds the password.
	proxyFile := writeFile(t, dir, "proxy", fixtureProxy("[::1")+"\n")
	var stdout, stderr strings.Builder
	if code := run(context.Background(), []string{"load", "gap", "--proxy-file", proxyFile, "--host", "example.com"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dev-airlock-stands load gap exited %d; stderr: %s", code, stderr.String())
	}
	var result struct{ Error string }
	if err := json.Unmarshal([]byte(stdout.String()), &result); err != nil || !strings.Contains(result.Error, "--proxy-file") {
		t.Fatalf("load gap answered %q (%v); want an error naming --proxy-file", stdout.String(), err)
	}
	if strings.Contains(stdout.String()+stderr.String(), fixturePassword) {
		t.Errorf("the error repeats the proxy password: %s%s", stdout.String(), stderr.String())
	}
}

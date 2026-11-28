package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// azureFilterRefusal is the answer the eval's grok-4.7 deployment gave a call its jailbreak shield
// refused (chat-calls.log of the c13 rerun): a 400 holding one stream chunk.
const azureFilterRefusal = `data: {"created":1,"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"content_filter","index":0,` +
	`"content_filter_results":{"error":{"code":"content_filter","message":"Response content blocked by label 'Jailbreak'."}}}]}` + "\n\n"

// TestTheProxyKeepsTheBodyOfACallTheContentFilterRefused: a call the deployment's content filter
// refused keeps the request body it sent, byte for byte, as refused/1.json under the proxy's dir,
// and its line in chat-calls.log names that file, so the refused call can be replayed: the server
// logs no request body, and without it which words the filter refused was not known. A call the
// deployment answered keeps nothing, nor does one it refused for another cause, and no header is
// written beside the body. The test sets SSL_CERT_FILE, so it does not run in parallel.
func TestTheProxyKeepsTheBodyOfACallTheContentFilterRefused(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("refuse me")):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, azureFilterRefusal)
		case bytes.Contains(body, []byte("too long")):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"too long"}}`)
		default:
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`+"\n\n")
		}
	}))
	t.Cleanup(upstream.Close)
	authorities := filepath.Join(t.TempDir(), "system.pem")
	if err := os.WriteFile(authorities, []byte("# no system authority is needed: the upstream is plain http\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", authorities)
	target, err := url.Parse(upstream.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	proxy, err := startChatProxy(target, newLimiter(1<<30), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.close)
	bundle, err := os.ReadFile(proxy.bundle)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(bundle)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	refusedBody := `{"model":"grok-4.7","messages":[{"role":"system","content":"rules"},{"role":"user","content":"refuse me"}],"stream":true}`
	for _, body := range []string{
		`{"model":"grok-4.7","messages":[{"role":"user","content":"answer me"}],"stream":true}`,
		refusedBody,
		`{"model":"grok-4.7","messages":[{"role":"user","content":"too long"}],"stream":true}`,
	} {
		request, err := http.NewRequest(http.MethodPost, proxy.endpoint()+"/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		// A sign-in the proxy passes on and the kept body must not hold.
		request.Header.Set("Authorization", "Bearer "+strings.Repeat("t", 16))
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	// Shutdown returns once every handler returned, and each writes its line before it does.
	if err := proxy.server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(filepath.Join(dir, "refused", "1.json"))
	if err != nil {
		t.Fatalf("the refused call's body was not kept: %v", err)
	}
	if string(kept) != refusedBody {
		t.Errorf("refused/1.json holds %q, want the request body as sent, %q", kept, refusedBody)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "refused")); err != nil || len(entries) != 1 {
		t.Errorf("refused/ holds %d files (%v), want the one refused call's", len(entries), err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "chat-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	var named []string
	for line := range strings.Lines(string(log)) {
		if strings.Contains(line, "refused_body=") {
			named = append(named, line)
		}
	}
	if len(named) != 1 || !strings.Contains(named[0], "status=400") || !strings.Contains(named[0], " refused_body=refused/1.json") {
		t.Errorf("chat-calls.log names a kept body on %q, want one line, the 400 of the filter's refusal, naming refused/1.json:\n%s", named, log)
	}
}

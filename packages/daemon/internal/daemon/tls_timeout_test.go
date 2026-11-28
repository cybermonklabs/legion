package daemon

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/tlstimeout"
	"github.com/sjawhar/envoy/tlstimeout/tlstimeouttest"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// R11 of dispatch://INFRA-3562, legion's own clients: a GitHub REST call of the daemon, through the
// REST client its boot puts on the rate counter's transport (countingTokens), to a GitHub that
// takes the connection and never answers TLS fails after http.DefaultTransport's 10 s with the
// error the counter's transport with no record gives the same call, and the record under it
// (tlsTimeouts) writes one line naming legion, the host and port alone and the handshake timeout
// as its event, with neither the token the call carried nor its path.
func TestTheDaemonsGitHubRESTClientRecordsATLSHandshakeTimeout(t *testing.T) {
	// Not parallel: it sets the package variable tlsTimeouts, and the REST client transport of the whole process (githubrest.SetTransport, in countingTokens).
	var stderr testlog.Buffer
	dir := t.TempDir()
	previous := tlsTimeouts
	tlsTimeouts = &tlstimeout.Recorder{
		Client: "legion", Stderr: &stderr, Bound: time.Second,
		StampDir: filepath.Join(dir, "stamps"), Curl: filepath.Join(dir, "no-curl"),
	}
	t.Cleanup(func() { tlsTimeouts = previous })
	host := tlstimeouttest.Silent(t)
	t.Cleanup(func() { githubrest.SetTransport(nil) })
	tokens, _ := countingTokens(config.GitHubApps{}, "https://"+host, quietLogger())
	t.Cleanup(tokens.Close)
	const token = "ghs_invented-installation-token-not-a-secret"
	call := func(client *http.Client) error {
		req, err := http.NewRequest(http.MethodGet, "https://"+host+"/repos/acme/widgets/pulls", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "token "+token)
		response, err := client.Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
		return err
	}
	// The same call through a counter's transport with no record, beside it, so the two wait out
	// their 10 s at once.
	bare := make(chan error, 1)
	go func() {
		bare <- call(&http.Client{Transport: githubrate.NewCounter().Transport(http.DefaultTransport)})
	}()
	recorded := call(githubrest.HTTPClient())
	unrecorded := testwait.Receive(t, "the call through a counter's transport with no record", bare)
	if !tlstimeout.IsTimeout(recorded) || unrecorded == nil || recorded.Error() != unrecorded.Error() {
		t.Errorf("the call answered %v, want net/http's TLS handshake timeout, as the call with no record answered: %v", recorded, unrecorded)
	}
	written := stderr.String()
	if strings.Count(written, "\n") != 1 || !strings.HasPrefix(written, "legion: tls handshake timeout recorded: host="+host+" event=handshake_timeout ") {
		t.Errorf("the client wrote %q, want one record line naming legion, host=%s and event=handshake_timeout", written, host)
	}
	for _, word := range []string{token, "Authorization", "/repos", "pulls"} {
		if strings.Contains(written, word) {
			t.Errorf("the record holds %q of the call: %q", word, written)
		}
	}
}

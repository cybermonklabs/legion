package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// dequeueListener is the Envoy listener as this test's daemon reaches it: the package's
// noticeListener, which takes every publish, with the 404 the listener answers for a role topic no
// live session holds (writeRoleHolderError, packages/envoy/cmd/listener/api.go, which notify
// decodes as ErrNoHolder) for each merger's topic, counted, until the test says a session holds
// that topic (holds).
type dequeueListener struct {
	notices  *noticeListener
	mu       sync.Mutex
	held     map[string]bool
	refusals map[string]int
}

// dequeueEnvoyToken is the bearer the daemon of newDequeueRig presents to its Envoy listener.
const dequeueEnvoyToken = "envoy-test-token"

// newDequeueListener starts the listener and names it in cfg as Envoy's, with dequeueEnvoyToken as
// the bearer the daemon presents to it. A request without that bearer is another process's: it is
// answered 404 and logged (fromTheDaemon), and the listener records nothing of it.
func newDequeueListener(t *testing.T, cfg *config.Config) *dequeueListener {
	t.Helper()
	l := &dequeueListener{notices: newNoticeListener(), held: map[string]bool{}, refusals: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromTheDaemon(t, w, r, dequeueEnvoyToken) {
			l.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfg.EnvoyURL = server.URL
	cfg.EnvoyTokenFile = filepath.Join(t.TempDir(), "envoy-token")
	if err := os.WriteFile(cfg.EnvoyTokenFile, []byte(dequeueEnvoyToken+"\n"), 0o600); err != nil {
		t.Fatalf("write the Envoy token: %v", err)
	}
	return l
}

// unheld makes the listener answer every publish to topic as it answers one to a role no live
// session holds, until holds.
func (l *dequeueListener) unheld(topic string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[topic] = false
}

// holds is the woken session taking topic: every later publish to it is delivered.
func (l *dequeueListener) holds(topic string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[topic] = true
}

// refused is how many publishes to topic the listener refused for want of a holder.
func (l *dequeueListener) refused(topic string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refusals[topic]
}

// accepted is every publish to topic the listener delivered, oldest first.
func (l *dequeueListener) accepted(topic string) []listenerPublish {
	return l.notices.acceptedOn(topic)
}

func (l *dequeueListener) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/v1/messages/publish" {
		l.notices.ServeHTTP(w, req)
		return
	}
	body, err := io.ReadAll(req.Body)
	var published listenerPublish
	if err == nil {
		err = json.Unmarshal(body, &published)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"read the publish: %v"}`, err), http.StatusBadRequest)
		return
	}
	l.mu.Lock()
	held, merger := l.held[published.Topic]
	refused := merger && !held
	if refused {
		l.refusals[published.Topic]++
	}
	l.mu.Unlock()
	if !refused {
		req.Body = io.NopCloser(bytes.NewReader(body))
		l.notices.ServeHTTP(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"error":"no live session holds the role","reason":"unclaimed"}`)
}

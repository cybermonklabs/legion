package sessionproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
)

// testCallTimeout is the bound of one request to Google in this file's rows, far below the 20 s
// each of their dials may take, so a dial that ends inside its own deadline was ended by it.
const testCallTimeout = 300 * time.Millisecond

// holdFirstAPIMint is the IAM Credentials API, which never answers the first generateAccessToken
// call for the Admin API's source (apiScopes): it holds that request until the request's context
// ends, as a server that took the request and never wrote its answer, and answers every other
// request as fake does. released ends a held request at the test's end where nothing else would.
type holdFirstAPIMint struct {
	fake     *iamCredentialsFake
	released chan struct{}

	mu   sync.Mutex
	held bool
}

func (h *holdFirstAPIMint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var asked struct {
		Scope []string `json:"scope"`
	}
	_ = json.Unmarshal(body, &asked)
	h.mu.Lock()
	hold := !h.held && slices.Equal(asked.Scope, apiScopes)
	h.held = h.held || hold
	h.mu.Unlock()
	if hold {
		holdUntilEnded(w, r, h.released)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.fake.ServeHTTP(w, r)
}

// holdFirstConnectSettings is the Cloud SQL Admin API, which never answers the first
// connectSettings call: it holds that request as holdFirstAPIMint does, and hands every other
// request to fake.
type holdFirstConnectSettings struct {
	fake     *adminAPIFake
	released chan struct{}

	mu   sync.Mutex
	held bool
}

func (h *holdFirstConnectSettings) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	hold := !h.held && r.Method == http.MethodGet && r.URL.Path == connectSettingsPath
	h.held = h.held || hold
	h.mu.Unlock()
	if hold {
		holdUntilEnded(w, r, h.released)
		return
	}
	h.fake.ServeHTTP(w, r)
}

// holdUntilEnded holds r until its context ends or released closes, then answers 504.
func holdUntilEnded(w http.ResponseWriter, r *http.Request, released <-chan struct{}) {
	select {
	case <-r.Context().Done():
	case <-released:
	}
	http.Error(w, "the stand-in never answered", http.StatusGatewayTimeout)
}

// requireTheBoundEndsTheHeldCall builds a real connector with connectorOptions for signer, which
// reaches Google at google (callTimeout testCallTimeout) and the Admin API at admin, and dials
// testInstance twice, each within 20 s. A stand-in holds one request of the first dial's refresh,
// which held names, so that dial must fail while its own deadline is still ahead: the request's
// bound ended it. The second dial's refresh is answered, so it must end with the connector's
// refusal of an instance with no private address: nothing the held request took is left held.
func requireTheBoundEndsTheHeldCall(t *testing.T, signer Signer, google googleEndpoints, admin, held string) {
	t.Helper()
	const noPrivateIP = `instance does not have IP of type "PRIVATE"`
	google.callTimeout = testCallTimeout
	opts, err := connectorOptions(context.Background(), signer, google)
	if err != nil {
		t.Fatalf("connectorOptions: %v", err)
	}
	connector, err := cloudsqlconn.NewDialer(context.Background(), append(opts, cloudsqlconn.WithAdminAPIEndpoint(admin+"/"))...)
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	defer func() { _ = connector.Close() }()
	dialer := connectorDialer{dialer: connector}

	first, cancelFirst := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelFirst()
	conn, err := dialer.Dial(first, testInstance)
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("the first dial answered a connection, though %s never answered", held)
	}
	if err == nil {
		t.Fatal("the first dial answered neither a connection nor an error")
	}
	if first.Err() != nil {
		t.Fatalf("the first dial ended with its own deadline (%v): %s, which never answered, held it; want that request ended at its bound of %s", err, held, testCallTimeout)
	}

	second, cancelSecond := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelSecond()
	if conn, err := dialer.Dial(second, testInstance); conn != nil {
		_ = conn.Close()
		t.Fatal("the second dial answered a connection to an instance with no private address")
	} else if err == nil || !strings.Contains(err.Error(), noPrivateIP) {
		t.Fatalf("the second dial = %v, want the connector's refusal %q: a dial after the request that never answered asks again and reaches the Admin API", err, noPrivateIP)
	}
}

// A request to Google that never answers ends at its bound (googleEndpoints.timeout), so it cannot
// hold its token source, and every later dial's certificate refresh behind it, for as long as the
// proxy serves: a token source reads no dial's context, so no dial's bound ends its request. The
// first dial's refresh asks IAM Credentials for the Admin API's token, which the stand-in never
// answers; the next dial mints the token again, which the stand-in answers. The real connector and
// the real impersonation library run against fakes of the pod's credentials, IAM Credentials and
// the Admin API. The test sets an environment variable, so it is not parallel.
func TestAGoogleCallThatNeverAnswersEndsAtItsBound(t *testing.T) {
	usePodCredentials(t)
	iam := &holdFirstAPIMint{fake: &iamCredentialsFake{t: t}, released: make(chan struct{})}
	t.Cleanup(func() { close(iam.released) })
	requireTheBoundEndsTheHeldCall(t, Signer{Account: "legion-sessions@example-project.iam.gserviceaccount.com"},
		googleEndpoints{iam: handlerTransport{iam}}, newAdminAPIFake(t).url, "the Admin API's token")
}

// An Admin API call that never answers ends at the same bound, whatever context it runs under:
// the Admin API client's own timeout holds it. The first dial's refresh asks for the instance's
// connect settings, which the stand-in never answers; the next dial's are answered. The proxy
// signs in as the pod itself, so no IAM Credentials call is made. The test sets an environment
// variable, so it is not parallel.
func TestAnAdminAPICallThatNeverAnswersEndsAtItsBound(t *testing.T) {
	usePodCredentials(t)
	admin := &holdFirstConnectSettings{fake: newAdminAPIFake(t), released: make(chan struct{})}
	server := httptest.NewServer(admin)
	t.Cleanup(func() { stopServing(server) })
	// Registered after the server, so it runs first: a held request ends before the server's stop
	// waits on its handler.
	t.Cleanup(func() { close(admin.released) })
	requireTheBoundEndsTheHeldCall(t, Signer{}, googleEndpoints{}, server.URL, "the instance's connect settings")
}

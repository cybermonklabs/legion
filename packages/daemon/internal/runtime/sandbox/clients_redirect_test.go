package sandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
)

// originBearer is the bearer of the redirect rows' configuration. It holds the gate's marker
// not-a-secret-token, so the text a row captures shows it where a stand-in was sent it.
const originBearer = "kube-origin-bearer-" + oauth2calltest.Token

// originSecretPath is the API path of the Secret name in the runtime's namespace, which the rows
// read as the runtime reads a launcher's Secret.
func originSecretPath(name string) string {
	return "/api/v1/namespaces/" + testNamespace + "/secrets/" + name
}

// originAPI is a stand-in Kubernetes API server. It answers a request that carries no
// originBearer 401, and one that carries it as the Secret it reads: "moved" with a 307 to the URL
// moved makes of the request, "renamed" with a 307 to "ordinary" on its own origin, and
// "ordinary" with the Secret.
func originAPI(moved func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") != "Bearer "+originBearer:
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == originSecretPath("moved"):
			http.Redirect(w, r, moved(r), http.StatusTemporaryRedirect)
		case r.URL.Path == originSecretPath("renamed"):
			http.Redirect(w, r, originSecretPath("ordinary"), http.StatusTemporaryRedirect)
		case r.URL.Path == originSecretPath("ordinary"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(corev1.Secret{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
				ObjectMeta: metav1.ObjectMeta{Name: "ordinary", Namespace: testNamespace},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// originInCluster is the configuration rest.InClusterConfig makes of a pod's service account, its
// API server at host: the bearer it read from the account's token file, and the file, which
// client-go reads again for each request (transport.NewBearerAuthWithRefreshRoundTripper).
func originInCluster(t *testing.T, host string) *rest.Config {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(originBearer), 0o600); err != nil {
		t.Fatal(err)
	}
	return &rest.Config{Host: host, BearerToken: originBearer, BearerTokenFile: file}
}

// originReads reads the Secret name through each of the runtime's two client sets of rc
// (clients), and answers what each read answered.
func originReads(t *testing.T, rc *rest.Config, name string) ([]*corev1.Secret, []error) {
	t.Helper()
	c, err := clients(rc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var (
		secrets []*corev1.Secret
		errs    []error
	)
	for _, set := range []kubeClients{c.requests, c.informers} {
		secret, err := set.kube.CoreV1().Secrets(testNamespace).Get(ctx, name, metav1.GetOptions{})
		secrets, errs = append(secrets, secret), append(errs, err)
	}
	return secrets, errs
}

// requireRefused fails t unless each of the runtime's reads was refused, as Sent's stand-in
// answers 500.
func requireRefused(t *testing.T, errs []error) {
	t.Helper()
	for i, err := range errs {
		if err == nil {
			t.Errorf("read %d through the runtime's clients succeeded, want it refused with the stand-in's 500", i)
		}
	}
}

// requireDefaultChainSends is the twin of a row: it reads the Secret "moved" through client-go's
// own client of rc (kubernetes.NewForConfig, the chain client-go builds with no wrap of the
// runtime's), and fails t unless Sent's stand-in was sent the bearer.
func requireDefaultChainSends(t *testing.T, rc *rest.Config, sent func() string) {
	t.Helper()
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, _ = kube.CoreV1().Secrets(testNamespace).Get(ctx, "moved", metav1.GetOptions{})
	if text := sent(); !strings.Contains(text, originBearer) {
		t.Fatalf("client-go's own client followed the 307 and sent the stand-in no bearer, so the row cannot see one sent on: %q", text)
	}
}

// requireFollowed fails t unless Sent's stand-in was asked once for each of the runtime's two
// reads (the request set's with its timeout in the query): each client followed the 307 to it.
func requireFollowed(t *testing.T, text string) {
	t.Helper()
	if got := strings.Count(text, "GET "+originSecretPath("moved")); got != 2 {
		t.Errorf("the stand-in the Location names was asked %d times, want each client's 307 followed to it: %q", got, text)
	}
}

// TestTheRuntimesClientsSendTheirBearerToTheAPIServersOriginAlone: each client the runtime makes
// of the daemon's cluster configuration (requestConfig: the request set and the informer set)
// follows a redirect and sends its bearer on only to one on the API server's own origin: the same
// scheme, host and port. The configuration is the one rest.InClusterConfig makes (originInCluster);
// a kubeconfig's carries its bearer through the same round tripper of client-go.
//
// Whatever answers in the API server's place answers the read of a Secret with a 307 to a
// stand-in that keeps what it is sent (redirectstest.Sent), at the read's own path: on the same
// host name at another port (other-port), on another host name of the same address (other-host),
// and on the same host and port over plain http where the read asked over https (other-scheme).
// The bearer reaches that stand-in in the Authorization net/http copies onto the redirect's request
// to the same host name at any port or scheme, and, where net/http drops it on the way to another
// host name, in the Authorization client-go's bearer round tripper sets again. Each read follows
// the 307 and sends the stand-in no Authorization; it is refused with the stand-in's 500. Each
// twin shows client-go's own client of the same configuration sends the stand-in the bearer, so
// the row can see one sent on. Captures sandbox.redirect-authorization/<case>, what the stand-in
// was sent.
//
// The controls: an ordinary read of a Secret, which the stand-in answers only to the bearer,
// succeeds through each client (ordinary), and so does one the stand-in answers with a 307 to it
// on its own origin, followed with the bearer (same-origin).
func TestTheRuntimesClientsSendTheirBearerToTheAPIServersOriginAlone(t *testing.T) {
	target, sent := redirectstest.Sent(t)
	targetHost, targetPort, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatal(err)
	}
	// api is the stand-in at the API server's address: it answers the read of "moved" 307 to the
	// stand-in that keeps what it is sent, at the read's own path, on the host name the case names
	// (targetName).
	var targetName atomic.Pointer[string]
	api := httptest.NewServer(originAPI(func(r *http.Request) string {
		return "http://" + net.JoinHostPort(*targetName.Load(), targetPort) + r.URL.RequestURI()
	}))
	t.Cleanup(api.Close)
	if apiHost, _, _ := net.SplitHostPort(api.Listener.Addr().String()); apiHost != targetHost || api.Listener.Addr().String() == target {
		t.Fatalf("the stand-in is at %s and the API server at %s: the row is a redirect to the same host name at another port", target, api.Listener.Addr())
	}
	inCluster := originInCluster(t, api.URL)

	for _, c := range []struct{ name, host string }{
		{name: "other-port", host: targetHost},
		// localhost is another host name of the loopback address the stand-in listens on, which
		// net/http drops the Authorization on the way to.
		{name: "other-host", host: "localhost"},
	} {
		t.Run("twin-"+c.name, func(t *testing.T) {
			targetName.Store(&c.host)
			requireDefaultChainSends(t, inCluster, sent)
		})
		t.Run(c.name, func(t *testing.T) {
			targetName.Store(&c.host)
			_, errs := originReads(t, inCluster, "moved")
			text := sent()
			oauth2calltest.RequireNoPiece(t, "sandbox.redirect-authorization/"+c.name, text, originBearer)
			requireFollowed(t, text)
			requireRefused(t, errs)
		})
	}

	t.Run("ordinary", func(t *testing.T) {
		secrets, errs := originReads(t, inCluster, "ordinary")
		for i, err := range errs {
			if err != nil || secrets[i].Name != "ordinary" {
				t.Errorf("read %d of the Secret through the runtime's clients = %v, %v; want the Secret the stand-in answers to the bearer", i, secrets[i], err)
			}
		}
	})
	t.Run("same-origin", func(t *testing.T) {
		secrets, errs := originReads(t, inCluster, "renamed")
		for i, err := range errs {
			if err != nil || secrets[i].Name != "ordinary" {
				t.Errorf("read %d of a Secret the stand-in moved on its own origin = %v, %v; want the 307 followed with the bearer to the Secret", i, secrets[i], err)
			}
		}
	})

	// secure is the stand-in at the API server's address over https: it answers the read of
	// "moved", which must carry the bearer, 307 to plain http on its own host and port. downgraded
	// is the base transport of the configuration: it makes each https connection to that address
	// with secure, and each plain one with the stand-in that keeps what it is sent. client-go
	// builds every round tripper of the configuration on it, the runtime's wrap among them.
	secure := httptest.NewTLSServer(originAPI(func(r *http.Request) string {
		return "http://" + r.Host + r.URL.RequestURI()
	}))
	t.Cleanup(secure.Close)
	secureHost := secure.Listener.Addr().String()
	roots := x509.NewCertPool()
	roots.AddCert(secure.Certificate())
	dialer := &net.Dialer{}
	downgraded := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == secureHost {
				addr = target
			}
			return dialer.DialContext(ctx, network, addr)
		},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, _ := net.SplitHostPort(addr)
			secured := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: host})
			if err := secured.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return secured, nil
		},
	}
	t.Cleanup(downgraded.CloseIdleConnections)
	overTLS := &rest.Config{Host: secure.URL, BearerToken: originBearer, Transport: downgraded}
	t.Run("twin-other-scheme", func(t *testing.T) {
		requireDefaultChainSends(t, overTLS, sent)
	})
	t.Run("other-scheme", func(t *testing.T) {
		_, errs := originReads(t, overTLS, "moved")
		text := sent()
		oauth2calltest.RequireNoPiece(t, "sandbox.redirect-authorization/other-scheme", text, originBearer)
		requireFollowed(t, text)
		requireRefused(t, errs)
	})
}

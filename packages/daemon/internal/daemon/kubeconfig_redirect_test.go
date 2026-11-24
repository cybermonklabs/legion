package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/envoy/redirects/redirectstest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// kubeconfigRedirectBearer is the token of the redirect row's kubeconfig user. It holds the gate's
// marker not-a-secret-token, so the text the row captures shows it where a stand-in was sent it.
const kubeconfigRedirectBearer = "kubeconfig-bearer-" + oauth2calltest.Token

// TestTheKubeconfigsBearerGoesToItsClustersOriginAlone: runtime.kubernetes names a kubeconfig
// whose user's token holds the gate's marker, and the boot's cluster check (sandbox.CheckInstalled)
// reads the Sandbox CRD and the controller's Deployment through the client kubeClient makes of it.
// The cluster's server is https, its authority in the kubeconfig: clientcmd reads a user's
// credentials into the configuration only for a server reached over TLS (DirectClientConfig's
// ClientConfig, rest.IsConfigTransportTLS).
//
// Whatever answers at the cluster's server answers the CRD's read with a 307 to a stand-in that
// keeps what it is sent (redirectstest.Sent), at the read's own path, on the same host name at
// another port over plain http, which net/http copies the Authorization onto the redirect's request
// to. The check follows it and sends the stand-in no Authorization (kubeconfig); it is refused with
// the stand-in's 500. The twin is client-go's own client of the same configuration
// (dynamic.NewForConfig), which sends the stand-in the bearer, so the row can see one sent on.
// Captures daemon.kube-redirect-authorization/kubeconfig, what the stand-in was sent.
//
// The control is the check answered as an installed Agent Sandbox (ordinary): each read, the
// Deployment's an ordinary read of a namespaced object, which the stand-in answers only to the
// bearer, succeeds, and the check passes.
func TestTheKubeconfigsBearerGoesToItsClustersOriginAlone(t *testing.T) {
	const (
		crdPath        = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/sandboxes.agents.x-k8s.io"
		deploymentPath = "/apis/apps/v1/namespaces/agent-sandbox-system/deployments/agent-sandbox-controller"
	)
	target, sent := redirectstest.Sent(t)
	// moving is whether the stand-in at the cluster's server answers the CRD's read with the 307,
	// or with the CRD.
	var moving atomic.Bool
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		switch {
		case r.Header.Get("Authorization") != "Bearer "+kubeconfigRedirectBearer:
			w.WriteHeader(http.StatusUnauthorized)
			return
		case r.URL.Path == crdPath && moving.Load():
			http.Redirect(w, r, "http://"+target+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		case r.URL.Path == crdPath:
			body = map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"metadata": map[string]any{"name": "sandboxes.agents.x-k8s.io"},
				"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1beta1", "served": true}}}}
		case r.URL.Path == deploymentPath:
			body = map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "agent-sandbox-controller", "namespace": "agent-sandbox-system"},
				"status":   map[string]any{"availableReplicas": 1}}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(api.Close)
	if apiHost, _, _ := net.SplitHostPort(api.Listener.Addr().String()); !strings.HasPrefix(target, apiHost+":") || api.Listener.Addr().String() == target {
		t.Fatalf("the stand-in is at %s and the cluster's server at %s: the row is a redirect to the same host name at another port", target, api.Listener.Addr())
	}
	file := filepath.Join(t.TempDir(), "kubeconfig")
	authority := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}))
	kubeconfig := "apiVersion: v1\nkind: Config\n" +
		"clusters: [{name: example, cluster: {server: \"" + api.URL + "\", certificate-authority-data: \"" + authority + "\"}}]\n" +
		"users: [{name: legion-daemon, user: {token: \"" + kubeconfigRedirectBearer + "\"}}]\n" +
		"contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]\n" +
		"current-context: legion-daemon\n"
	if err := os.WriteFile(file, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, err := kubeClient(config.Kubernetes{Kubeconfig: file})
	if err != nil {
		t.Fatal(err)
	}
	bound := func(t *testing.T) context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		t.Cleanup(cancel)
		return ctx
	}

	t.Run("twin", func(t *testing.T) {
		moving.Store(true)
		dyn, err := dynamic.NewForConfig(rc)
		if err != nil {
			t.Fatal(err)
		}
		crds := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
		_, _ = dyn.Resource(crds).Get(bound(t), agentSandbox.CRD, metav1.GetOptions{})
		if text := sent(); !strings.Contains(text, kubeconfigRedirectBearer) {
			t.Fatalf("client-go's own client followed the 307 and sent the stand-in no bearer, so the row cannot see one sent on: %q", text)
		}
	})
	t.Run("kubeconfig", func(t *testing.T) {
		moving.Store(true)
		err := sandbox.CheckInstalled(bound(t), rc, agentSandbox)
		text := sent()
		oauth2calltest.RequireNoPiece(t, "daemon.kube-redirect-authorization/kubeconfig", text, kubeconfigRedirectBearer)
		if !strings.HasPrefix(text, "GET "+crdPath) {
			t.Errorf("the stand-in the Location names was sent %q, want the CRD's 307 followed to it", text)
		}
		if err == nil {
			t.Error("the cluster check passed though the CRD's read was refused with the stand-in's 500")
		}
	})
	t.Run("ordinary", func(t *testing.T) {
		moving.Store(false)
		if err := sandbox.CheckInstalled(bound(t), rc, agentSandbox); err != nil {
			t.Errorf("the cluster check of an installed Agent Sandbox, which the stand-in answers only to the bearer, failed: %v", err)
		}
	})
}

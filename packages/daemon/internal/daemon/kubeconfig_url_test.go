package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// kubeconfigCredential is the user information a row's kubeconfig writes into a URL: a made-up
// user and the made-up password oauth2calltest.RequireNone looks for.
const kubeconfigCredential = "kube-user:not-a-secret-password"

// TestAKubeconfigURLClientGoCannotParsePrintsNoneOfItsUserInformation: runtime.kubernetes'
// kubeconfig names its cluster by a URL that client-go cannot parse and that carries user
// information: the cluster's server, read with no context configured, so the kubeconfig's current
// one (server), and its proxy-url, read under the configured context (proxy-url). client-go
// refuses either as written: rest's DefaultServerURL hands back url.Parse's error, which quotes the
// URL, user information and all, at the first client the boot builds (the cluster check,
// sandbox.CheckInstalled), and clientcmd's validation quotes the proxy-url in ClientConfig's
// error. The boot's refusal (kubeClient, or the cluster check where kubeClient took the file)
// names the kubeconfig and holds none of the user information. Captures
// kubeerror.daemon/kubeconfig-<case>.
func TestAKubeconfigURLClientGoCannotParsePrintsNoneOfItsUserInformation(t *testing.T) {
	for _, c := range []struct {
		name, context, server, proxy string
	}{
		{name: "server", server: "https://" + kubeconfigCredential + "@192.0.2.1:%zz"},
		{name: "proxy-url", context: "legion-daemon", server: "https://192.0.2.1",
			proxy: "http://" + kubeconfigCredential + "@proxy.example:%zz"},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "kubeconfig")
			cluster := "server: \"" + c.server + "\""
			if c.proxy != "" {
				cluster += ", proxy-url: \"" + c.proxy + "\""
			}
			kubeconfig := "apiVersion: v1\nkind: Config\n" +
				"clusters: [{name: example, cluster: {" + cluster + "}}]\n" +
				"users: [{name: legion-daemon, user: {token: placeholder}}]\n" +
				"contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]\n" +
				"current-context: legion-daemon\n"
			if err := os.WriteFile(file, []byte(kubeconfig), 0o600); err != nil {
				t.Fatal(err)
			}

			rc, err := kubeClient(config.Kubernetes{Kubeconfig: file, Context: c.context})
			if err == nil {
				err = sandbox.CheckInstalled(context.Background(), rc, agentSandbox)
			}

			if err == nil {
				t.Fatal("the boot took a kubeconfig whose URL client-go cannot parse")
			}
			oauth2calltest.RequireNone(t, "kubeerror.daemon/kubeconfig-"+c.name, err.Error())
			if !strings.Contains(err.Error(), "runtime.kubernetes.kubeconfig "+file+": ") {
				t.Errorf("the refusal is %q, want it to name the kubeconfig %s", err, file)
			}
		})
	}
}

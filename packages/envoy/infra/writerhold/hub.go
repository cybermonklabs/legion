package writerhold

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	compute "google.golang.org/api/compute/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sjawhar/envoy/infra/seed"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// HubFlags are the flags by which a command of the hold reaches the secrets broker's hold routes
// and the clustered hub: `envoy-infra hold` and `envoy-infra buckets` take the same ones.
type HubFlags struct {
	// BrokerURL is the secrets broker's base URL, TokenFile the operator's ServiceAccount token
	// for the broker's NATS audience, read at each use, and VerifyKeys the broker's
	// BROKER_NATS_HOLD_SIGNING_KEYS, the keys the hold's rows and records verify under.
	BrokerURL, TokenFile, VerifyKeys string
	// KeySecret is the Secret Manager secret of the operator's signing key
	// (projects/<p>/secrets/SigningKeySecret), which every command that writes the hold reads.
	KeySecret string
	// MintersReport is operator-token-minters.sh's --report file, which a command that prepares a
	// hold checks (PreflightMinters).
	MintersReport string
	// NATS are the hub's client URLs, and ProbeTokenFile the hold probe's platform token.
	NATS           []string
	ProbeTokenFile string
	// Namespace, StatefulSet and ConfigMap name the clustered hub's Kubernetes objects.
	Namespace, StatefulSet, ConfigMap string
}

// Register defines the flags on flags, with the hub's names as their defaults.
func (h *HubFlags) Register(flags *flag.FlagSet) {
	h.Namespace, h.StatefulSet, h.ConfigMap = DefaultNamespace, DefaultStatefulSet, DefaultConfigMap
	flags.StringVar(&h.BrokerURL, "broker-url", "", "the secrets broker's base URL")
	flags.StringVar(&h.TokenFile, "token-file", "", "the operator's ServiceAccount token")
	flags.StringVar(&h.VerifyKeys, "verify-keys", "", "BROKER_NATS_HOLD_SIGNING_KEYS")
	flags.StringVar(&h.KeySecret, "signing-key-secret", "", "projects/<project>/secrets/"+SigningKeySecret)
	flags.StringVar(&h.MintersReport, "minters-report", "", "operator-token-minters.sh's --report file")
	flags.Func("nats", "a hub client URL, repeatable", func(v string) error { h.NATS = append(h.NATS, v); return nil })
	flags.StringVar(&h.ProbeTokenFile, "probe-token-file", "", "the hold probe's platform token")
	flags.StringVar(&h.Namespace, "kube-namespace", h.Namespace, "the hub's namespace")
	flags.StringVar(&h.StatefulSet, "statefulset", h.StatefulSet, "the hub's StatefulSet")
	flags.StringVar(&h.ConfigMap, "configmap", h.ConfigMap, "the hub's ConfigMap")
}

// Records opens the operator's client of the broker's hold routes, with the signing key from
// Secret Manager when signing.
func (h HubFlags) Records(ctx context.Context, signing bool) (*holdrecord.Client, error) {
	return openRecords(ctx, h, signing, openKeys)
}

// Operator opens the operator's reach of the hub: its Kubernetes objects through the in-cluster
// configuration (the operator's Job) or KUBECONFIG's, the Compute API for its disks, and its NATS
// client URLs, with the operator's and the probe's credentials.
func (h HubFlags) Operator(ctx context.Context) (*Operator, error) { return openOperator(ctx, h) }

// signingKey reads the private half from store's served version of secret.
func signingKey(ctx context.Context, store KeyStore, secret string) (ed25519.PrivateKey, error) {
	_, value, err := store.Current(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("read the signing key from %s: %w", secret, err)
	}
	defer clear(value)
	seedBytes, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(string(value)))
	if err != nil || len(seedBytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s's served version is not the unpadded base64url of a %d-byte Ed25519 seed (not echoed)", secret, ed25519.SeedSize)
	}
	defer clear(seedBytes)
	return ed25519.NewKeyFromSeed(seedBytes), nil
}

// openRecords opens the operator's client of h's broker: the token read from h.TokenFile at each
// request, the verify keys parsed from h.VerifyKeys, and, when signing, the private half from the
// key store.
func openRecords(ctx context.Context, h HubFlags, signing bool, keys func(context.Context) (KeyStore, error)) (*holdrecord.Client, error) {
	verify, err := signedrecord.ParsePublicKeys("--verify-keys", h.VerifyKeys)
	if err != nil {
		return nil, err
	}
	cfg := holdrecord.Config{
		BrokerURL:  h.BrokerURL,
		Token:      tokenFromFile(h.TokenFile),
		VerifyKeys: verify,
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
	if signing {
		store, err := keys(ctx)
		if err != nil {
			return nil, err
		}
		if cfg.SigningKey, err = signingKey(ctx, store, h.KeySecret); err != nil {
			return nil, err
		}
	}
	return holdrecord.New(cfg)
}

// tokenFromFile reads the token at path at each call: the kubelet rewrites a projected token in
// place.
func tokenFromFile(path string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the token file %s: %w", path, err)
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("the token file %s is empty", path)
		}
		return token, nil
	}
}

// openKeys opens Secret Manager, as `envoy-infra seed --store gcp-secret-manager` does.
func openKeys(ctx context.Context) (KeyStore, error) { return seed.OpenGCP(ctx, "", "") }

// ConnectOperator connects to urls as the operator: its platform token from tokenFile, read at each
// connect, in CONNECT's password (natscred.Service), with its reply inboxes under
// natsgrant.OperatorInbox, the one prefix the operator's grant subscribes. The auth service decides
// it under the hold row's phase at that moment, and decides it again at each reconnect.
func ConnectOperator(urls []string, tokenFile, name string) (*nats.Conn, error) {
	credential, _ := natscred.Service(natscred.ServiceFile(tokenFile))
	return nats.Connect(strings.Join(urls, ","), credential, nats.CustomInboxPrefix(natsgrant.OperatorInbox),
		nats.Name(name), nats.Timeout(10*time.Second), nats.MaxReconnects(-1))
}

// openOperator is the real Operator of h.
func openOperator(ctx context.Context, h HubFlags) (*Operator, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("no Kubernetes configuration, in the cluster or in KUBECONFIG: %w", err)
		}
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("open the Compute API: %w", err)
	}
	return &Operator{
		Hub: KubeHub{Client: client, Namespace: h.Namespace, StatefulSet: h.StatefulSet, ConfigMap: h.ConfigMap,
			Forward: APIForwarder{Config: config}},
		Disks: ComputeDisks{Service: computeService},
		URLs:  h.NATS,
		Connect: func(context.Context) (*nats.Conn, error) {
			return ConnectOperator(h.NATS, h.TokenFile, "envoy-infra hold")
		},
		Probe: func(_ context.Context, url string) error {
			credential, _ := natscred.Service(natscred.ServiceFile(h.ProbeTokenFile))
			nc, err := nats.Connect(url, credential, nats.Name("envoy-infra hold probe"), nats.NoReconnect(), nats.Timeout(10*time.Second))
			if err == nil {
				nc.Close()
			}
			return err
		},
	}, nil
}

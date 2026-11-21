package writerhold

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// The clustered hub as the deployment names it (the deployment repository's envoy-cluster program, through
// gcp:ClusteredNatsHub): namespace envoy, the StatefulSet envoy-nats, whose pods are envoy-nats-<i>,
// and the chart's ConfigMap envoy-nats-config, which holds the configuration each pod runs.
const (
	DefaultNamespace   = "envoy"
	DefaultStatefulSet = "envoy-nats"
	DefaultConfigMap   = "envoy-nats-config"
	// MonitorPort is nats-server's HTTP monitoring port, ClientPort its client port.
	MonitorPort = 8222
	ClientPort  = 4222
)

// Forwarder opens a local address that reaches a port of a pod, as kubectl port-forward does
// through the API server's pods/portforward subresource. The hold reaches each hub pod's monitoring
// and client ports this way alone: the hub's NetworkPolicy admits no pod to 8222, and the operator's
// Role grants port-forward to the hub's pods by name (resourceNames).
type Forwarder interface {
	// Forward opens address, host:port, which reaches port of pod in namespace until stop is
	// called.
	Forward(ctx context.Context, namespace, pod string, port int) (address string, stop func(), err error)
}

// KubeHub is the clustered hub, read through the Kubernetes API: its StatefulSet's pods are the
// declared servers, and each is reached through Forward.
type KubeHub struct {
	Client      kubernetes.Interface
	Namespace   string
	StatefulSet string
	ConfigMap   string
	Forward     Forwarder
	// HTTP reads each monitoring port through its forwarded address; http.DefaultClient when nil.
	HTTP *http.Client
}

// Pods are the declared servers: the StatefulSet's pods by its own spec (statefulSetPods). A
// StatefulSet that cannot be read, or that declares no replica count, is UNKNOWN.
func (h KubeHub) Pods(ctx context.Context) ([]string, error) {
	set, err := h.Client.AppsV1().StatefulSets(h.Namespace).Get(ctx, h.StatefulSet, metav1.GetOptions{})
	if err != nil {
		return nil, unknownf(StepPreflight, err, "read StatefulSet %s/%s", h.Namespace, h.StatefulSet)
	}
	return statefulSetPods(set)
}

// statefulSetPods are set's pods, by the ordinals its controller gives them: replicas of them, from
// spec.ordinals.start (0 when unset). A StatefulSet that declares no replica count is UNKNOWN.
func statefulSetPods(set *appsv1.StatefulSet) ([]string, error) {
	if set.Spec.Replicas == nil || *set.Spec.Replicas < 1 {
		return nil, unknownf(StepPreflight, nil, "StatefulSet %s/%s declares no replica count", set.Namespace, set.Name)
	}
	start := int32(0)
	if set.Spec.Ordinals != nil {
		start = set.Spec.Ordinals.Start
	}
	pods := make([]string, *set.Spec.Replicas)
	for i := range pods {
		pods[i] = set.Name + "-" + strconv.Itoa(int(start)+i)
	}
	return pods, nil
}

// Monitor opens pod's monitoring port, which stop closes.
func (h KubeHub) Monitor(ctx context.Context, pod string) (Monitor, func(), error) {
	address, stop, err := h.Forward.Forward(ctx, h.Namespace, pod, MonitorPort)
	if err != nil {
		return nil, nil, fmt.Errorf("forward %s's monitoring port: %w", pod, err)
	}
	return NewHTTPMonitor("http://"+address, h.HTTP), stop, nil
}

// ClientURL opens pod's own client port alone, the address the probe dials to show that this one
// server refuses it, which stop closes.
func (h KubeHub) ClientURL(ctx context.Context, pod string) (string, func(), error) {
	address, stop, err := h.Forward.Forward(ctx, h.Namespace, pod, ClientPort)
	if err != nil {
		return "", nil, fmt.Errorf("forward %s's client port: %w", pod, err)
	}
	return "nats://" + address, stop, nil
}

// APIForwarder is the Forwarder of the API server config names: kubectl port-forward's own path,
// a WebSocket tunnel with SPDY as the fallback (client-go's tools/portforward), on a local port the
// kernel picks on 127.0.0.1.
type APIForwarder struct {
	Config *rest.Config
}

// forwardReady bounds how long a forward may take to listen.
const forwardReady = 30 * time.Second

func (f APIForwarder) Forward(ctx context.Context, namespace, pod string, port int) (string, func(), error) {
	if f.Config == nil {
		return "", nil, errors.New("the forwarder has no API server configuration")
	}
	target, err := url.Parse(f.Config.Host)
	if err != nil {
		return "", nil, fmt.Errorf("the API server address %q: %w", f.Config.Host, err)
	}
	target.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", url.PathEscape(namespace), url.PathEscape(pod))
	transport, upgrader, err := spdy.RoundTripperFor(f.Config)
	if err != nil {
		return "", nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, target)
	tunnel, err := portforward.NewSPDYOverWebsocketDialer(target, f.Config)
	if err != nil {
		return "", nil, err
	}
	dialer = portforward.NewFallbackDialer(tunnel, dialer, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	stopCh, ready := make(chan struct{}), make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:" + strconv.Itoa(port)}, stopCh, ready, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	failed := make(chan error, 1)
	go func() { failed <- forwarder.ForwardPorts() }()
	stop := func() { close(stopCh) }
	select {
	case <-ready:
	case err := <-failed:
		return "", nil, fmt.Errorf("port-forward to %s/%s:%d: %w", namespace, pod, port, err)
	case <-time.After(forwardReady):
		stop()
		return "", nil, fmt.Errorf("port-forward to %s/%s:%d did not listen within %s", namespace, pod, port, forwardReady)
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		stop()
		return "", nil, fmt.Errorf("port-forward to %s/%s:%d answered no local port: %v", namespace, pod, port, err)
	}
	return "127.0.0.1:" + strconv.Itoa(int(ports[0].Local)), stop, nil
}

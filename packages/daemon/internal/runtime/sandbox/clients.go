package sandbox

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/client-go/util/flowcontrol"
)

// clientQPS and clientBurst are the rate the runtime's Kubernetes requests are paced at, sized for
// a few hundred roles: a restart has every live role's launcher say hello again, each costing a read
// of its launcher Secret (launcherCredential), beside the requests of the launches in flight.
// docs/kubernetes.md, "The daemon's Kubernetes request rate", gives the arithmetic. A watch is never
// paced (client-go's Request.Watch).
const (
	clientQPS   = 50
	clientBurst = 100
)

// kubeClients are a dynamic and a typed client of one client configuration.
type kubeClients struct {
	dyn  dynamic.Interface
	kube kubernetes.Interface
}

// runtimeClients are the runtime's two client sets: informers for its informers' lists and
// watches, and requests for every other request it makes (clientConfigs).
type runtimeClients struct {
	informers, requests kubeClients
}

// clients are the runtime's client sets of rc (clientConfigs).
func clients(rc *rest.Config) (runtimeClients, error) {
	informerConfig, requestConfig := clientConfigs(rc)
	informers, err := clientsOf(informerConfig)
	if err != nil {
		return runtimeClients{}, err
	}
	requests, err := clientsOf(requestConfig)
	if err != nil {
		return runtimeClients{}, err
	}
	return runtimeClients{informers: informers, requests: requests}, nil
}

// clientConfigs are the runtime's two configurations of rc: the requests' (requestConfig), and the
// informers', which share the requests' limiter and have no timeout, since a timeout would end a
// watch.
func clientConfigs(rc *rest.Config) (informers, requests *rest.Config) {
	requests = requestConfig(rc)
	informers = rest.CopyConfig(requests)
	informers.Timeout = 0
	return informers, requests
}

// requestConfig is rc paced by a rate limiter of its own at clientQPS and clientBurst, each request
// bounded by apiTimeout, and its Authorization sent to rc's API server alone
// (keepAuthorizationOnOrigin). client-go waits on a client's limiter before it starts that client's
// timeout (rest/request.go, Request.request), so a request's wait to be let go is bounded by its
// caller's context alone, a launch's own deadline, and a launch the limiter holds back waits its
// turn rather than failing; the 30 s bound is the request's own once it is sent. Each call makes a
// limiter of its own: the runtime's two client sets share one (clientConfigs), and each boot check
// paces itself at the same rate (CheckInstalled, CensusLegacyIssueSandboxes). Every client the
// daemon makes of its cluster's configuration is made of such a copy (clients, CheckInstalled,
// CensusLegacyIssueSandboxes), so each carries the wrap.
func requestConfig(rc *rest.Config) *rest.Config {
	requests := rest.CopyConfig(rc)
	requests.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(clientQPS, clientBurst)
	requests.Timeout = apiTimeout
	requests.WrapTransport = transport.Wrappers(keepAuthorizationOnOrigin(requests), requests.WrapTransport)
	return requests
}

// keepAuthorizationOnOrigin is the wrap of rc's transport that sends a request's Authorization to
// rc's API server alone: a request to any other origin (scheme, host and port, as
// redirects.SameOrigin compares them) goes without it (originAuthorization). client-go follows a
// redirect as net/http's own policy does (rest.HTTPClientFor sets no CheckRedirect, and a
// rest.Config has no field for one), and its credential is set by the round trippers it builds
// below the client: net/http copies the Authorization onto a redirect's request to the same host
// name at any port and scheme (shouldCopyHeaderOnRedirect, net/http/client.go of go1.27.3, lines
// 700-705 and 1017-1039), and where it drops it, on the way to another host name, client-go's
// bearer round tripper sets it again, as it does on every request that carries none
// (transport/round_trippers.go of client-go v0.37.0, lines 310-324). So, but for the wrap, a
// redirect that whoever answers in the API server's place writes takes the bearer wherever it
// points. The redirect is still followed, without the bearer off the origin; one on the API
// server's own origin keeps it.
//
// The wrap is the innermost round tripper: client-go runs a configuration's WrapTransport below
// the bearer, basic-auth, exec and auth-provider round trippers it adds
// (transport.HTTPWrappersForConfig, rest.Config.TransportConfig), and requestConfig runs it below
// any wrap rc already has, so it sees the Authorization each of them sets. A Host client-go cannot
// parse leaves the origin empty, which no request is on, so each goes without Authorization;
// client-go builds no client of such a configuration (rest.RESTClientFor refuses it on the same
// parse, rest.DefaultServerUrlFor).
func keepAuthorizationOnOrigin(rc *rest.Config) transport.WrapperFunc {
	var origin url.URL
	if api, _, err := rest.DefaultServerUrlFor(rc); err == nil {
		origin = *api
	}
	return func(next http.RoundTripper) http.RoundTripper {
		return &originAuthorization{origin: origin, next: next}
	}
}

// originAuthorization is a round tripper that sends each request on to next, without its
// Authorization where the request's scheme or host and port are not origin's
// (keepAuthorizationOnOrigin).
type originAuthorization struct {
	origin url.URL
	next   http.RoundTripper
}

// RoundTrip sends req on, or a copy of it without its Authorization where it is off o's origin: a
// round tripper does not change the request it is handed.
func (o *originAuthorization) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" && (req.URL.Scheme != o.origin.Scheme || !strings.EqualFold(req.URL.Host, o.origin.Host)) {
		req = utilnet.CloneRequest(req)
		req.Header.Del("Authorization")
	}
	return o.next.RoundTrip(req)
}

// WrappedRoundTripper is the round tripper o sends on to, as each of client-go's own answers
// (utilnet.RoundTripperWrapper).
func (o *originAuthorization) WrappedRoundTripper() http.RoundTripper { return o.next }

func clientsOf(rc *rest.Config) (kubeClients, error) {
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return kubeClients{}, fmt.Errorf("sandbox runtime: dynamic client: %w", err)
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return kubeClients{}, fmt.Errorf("sandbox runtime: kubernetes client: %w", err)
	}
	return kubeClients{dyn: dyn, kube: kube}, nil
}

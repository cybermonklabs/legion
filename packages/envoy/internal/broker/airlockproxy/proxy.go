// Package airlockproxy is the broker's proxy port: the airlock itself. A command the broker
// released a name behind the airlock to holds a placeholder where the secret would be and
// points its HTTPS_PROXY here; this port holds the real value and puts it into each request the
// rules allow, as that request leaves.
//
// One connection is one live grant. The proxy's password is a placeholder of that grant
// (requests.Machine.AirlockConnection), and the connection may carry every airlock name the
// grant holds that its operator has not withheld. A CONNECT to a host one of those names is
// bound to is answered with a certificate this proxy's own authority signs, so each request on
// it is read: the decision and the swap are internal/broker/airlock's Decide and Rewrite, which
// a Lean model is proved against and a differential fuzz test ties to, and nothing here decides
// or swaps anything itself. A CONNECT to any other host is a plain tunnel: the proxy opens no
// TLS, reads nothing, and copies bytes.
//
// What this package adds to those two functions is everything around them: who the connection
// is (the grant), where it may dial (address.go), what the request looks like as a
// airlock.Request, which (name, slot) pair of a grant one request is handled for, where the
// real values come from, how long they are held and what ends them, and the line each call to a
// bound host logs.
package airlockproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/netaddr"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/fold"
	"github.com/sjawhar/envoy/internal/metrics"
)

// readHeaderTimeout bounds how long a connection on this port may take to send the head of a
// request. It is the one deadline either server here carries: a request deadline or a write
// timeout would cut a streamed answer - a long poll, a large artifact, a server-sent event
// stream - in the middle, and what bounds a connection that has gone quiet instead is the idle
// limit (BROKER_AIRLOCK_IDLE_SECONDS), which a streamed answer keeps pushing out.
const readHeaderTimeout = 30 * time.Second

// upstreamIdleTimeout and upstreamIdleConns are how long an upstream connection is kept for the
// next request to a service and how many per host. The proxy speaks to a handful of API hosts
// for every grant, so keeping a connection is the difference between one TLS handshake per pod
// and one per call.
const (
	upstreamIdleTimeout = 90 * time.Second
	upstreamIdleConns   = 32
)

// watchInterval is how often a call the proxy is carrying reads its grant again (watchCarried):
// a WebSocket the proxy joined, a gRPC call whose answers stream, a server-sent event stream, a
// long poll. No request is read on such a call once it has started, so this is the whole of how
// long one may carry bytes once its grant has ended, and fifteen seconds is short beside the
// shortest grant the broker issues. A call that ends within the interval reads nothing more. One
// that runs longer costs one connection read per interval, the three indexed reads
// AirlockConnection makes, which a pod holding a handful of long calls pays a few times a
// minute, and beside it a read of the policy version, which is a pointer: the values a pod holds
// are read again only once that version has moved.
const watchInterval = 15 * time.Second

// GrantSource is the reads of requests.Machine the proxy makes: one per connection, one per
// grant per pod, and the policy version that says whether what a pod holds was read under the
// policy in force now.
type GrantSource interface {
	// AirlockConnection is what one connection's password proves: the live grant behind it and
	// every name it may carry. Its own two refusals are this port's 407s, and every other
	// error of it is a read that did not happen (grantRefusal).
	AirlockConnection(ctx context.Context, placeholder string) (requests.AirlockConnection, error)
	// AirlockValues runs the full release checks and answers the real value of every slot of
	// every name, with when each name must be read or minted again, and the policy version it
	// ran under. The proxy calls it on a grant's first connection on this pod and keeps what
	// it answered, that version with it.
	AirlockValues(ctx context.Context, grantID string) (map[string]requests.AirlockSecret, string, error)
	// PolicyVersion is the version of the policy the broker holds now. The values a pod holds
	// are the ones a read made under one version, and a version that has moved since is what
	// says they must be read again: the broker's periodic reload reaches this and reaches no
	// value a pod already holds any other way.
	PolicyVersion() string
	// AirlockAccessPlaceholder is the placeholder that stands for one access token the proxy
	// exchanged a grant's federated token for (azure.go): minted on the first exchange of that
	// name and slot and answered again after, so a command that asks for one scope twice - and
	// a second pod that exchanges that scope for itself - reads back the same string.
	AirlockAccessPlaceholder(ctx context.Context, grantID, name, slot string) (string, error)
}

// Options is what a Server is built from.
type Options struct {
	// Rules is the airlock's rules file, read once at boot: which hosts each secret may reach
	// and in which headers.
	Rules airlock.Rules
	// Grants is the broker's own two reads, in production a *requests.Machine.
	Grants GrantSource
	// CA signs the certificate of each bound host.
	CA *Authority
	// Idle is BROKER_AIRLOCK_IDLE_SECONDS: how long one connection may go with no byte in
	// either direction. Zero or less leaves a connection open for as long as the command holds
	// it.
	Idle time.Duration
	// Watch is how often a carried call reads its grant again (watchCarried); zero or less is
	// watchInterval. A test sets a short one and ends a grant under an open WebSocket or
	// stream, rather than waiting out the real interval.
	Watch time.Duration
	// HashBudget is how many bytes of body the proxy holds at once across every connection
	// for the AWS calls it has to hash itself (aws.go); zero or less is hashedBodyBudget. A
	// test sets a small one and drives two requests at it, rather than sending the hundreds
	// of megabytes the real budget takes to fill.
	HashBudget int64
	// Log is where the line for each call to a bound host goes, and the line for each refusal of
	// this port. Nil is slog.Default().
	Log *slog.Logger
	// Answers is broker_answers_total (answer.NewAnswers), which each answer a handler of this
	// port writes is counted on under caller class airlock, beside the API port's answers; an
	// answer net/http writes itself before any handler runs is counted nowhere. Nil counts
	// nothing.
	Answers *metrics.Counter
	// Dial is how every upstream connection is made; nil is a net.Dialer. It is handed the
	// address the proxy resolved and checked, never a host name, so a replacement of it
	// cannot reach a name's second answer (address.go). A test replaces it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Now is the clock the proxy reads to tell a minted value that is about to stop working
	// from one that is not (mintAgainBefore); nil is time.Now. A test drives it rather than
	// waiting out a token's life.
	Now func() time.Time
	// Resolve answers the addresses a host has: the ones the refusal reads and the ones the
	// dial opens a socket to (checkedAddresses); nil is net.DefaultResolver's lookup. So a
	// host that resolves here reaches whatever this answers for it, and nothing below looks
	// the name up a second time. A test drives a public name at a stand-in service on
	// loopback with it, and BROKER_AIRLOCK_TEST_RESOLVE is it in the broker's local stack,
	// where a client that resolves a real name - az, the AWS client - has to reach a
	// stand-in.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// UpstreamRoots are the roots the proxy verifies a service's certificate against on its
	// own calls upstream; nil is the host's own roots, as any client uses. A test puts its
	// stand-in service's certificate in it, and BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE is it in
	// the broker's local stack, where the service behind a bound host is a stand-in with a
	// certificate of the run's own.
	UpstreamRoots *x509.CertPool
	// DialAllow is the host:port addresses the proxy may dial although the address they
	// resolve to is one it never dials - a loopback or private address. It is a list rather
	// than a switch because the broker's local stack needs both halves at once: the stand-in
	// services on loopback reachable, and the broker's own loopback port still refused.
	// BROKER_AIRLOCK_TEST_DIAL_ALLOW is it.
	DialAllow []string
	// ClusterRanges is BROKER_AIRLOCK_CLUSTER_RANGES: the broker cluster's own Service, pod and
	// node ranges. Such a range can be global unicast - a GKE Service range such as
	// 34.118.224.0/20 is - so none of localRule's fixed rules refuses an address in it, and a
	// tunnel there reaches a ClusterIP of the cluster the broker runs in. An address inside any
	// of them is refused as a private one is, after those fixed rules; DialAllow still opens
	// one it names. Nil refuses nothing beyond the fixed rules, which is a broker outside a
	// cluster.
	ClusterRanges []netip.Prefix
	// KeepValues keeps a grant's values across the reread of one of its names and past the
	// five-minute margin of a minted token's expiry, so a run can show what the airlock does
	// when neither happens. BROKER_AIRLOCK_TEST_KEEP_VALUES is it, the negative control of
	// the check that both halves belong to.
	KeepValues bool
	// NoKind turns off the azure kind's exchange of a command's token request and the AWS
	// signing, so an entry whose kind names one of them takes the plain header swap instead, and
	// a request carrying the federated token's placeholder in a header is still refused
	// (serveBound). BROKER_AIRLOCK_TEST_NO_KIND is it, the negative control of the check the two
	// kinds belong to.
	NoKind bool
	// AzureAuthority is the host of the token authority every exchange of the azure kind is
	// made at (azure.go): a host name in lower case with no port, which the host of a token
	// request's connection, ASCII lower-cased, must equal exactly, and "" for Microsoft's,
	// login.microsoftonline.com. A token request to any other host an azure entry binds is
	// refused, and so is an access slot that names one, since the exchange carries the broker's
	// federated token. BROKER_AIRLOCK_TEST_AZURE_AUTHORITY is it in the broker's local stack,
	// whose token authority is a stand-in.
	AzureAuthority string
}

// Server is the broker's proxy port: one HTTP CONNECT proxy whose password is a placeholder of a
// live grant.
type Server struct {
	rules  airlock.Rules
	grants GrantSource
	ca     *Authority
	idle   time.Duration
	watch  time.Duration
	log    *slog.Logger
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	now    func() time.Time

	// answers is Options.Answers, which record counts each answer a handler of the port writes
	// on.
	answers *metrics.Counter
	// resolve answers the addresses a host has, the ones refuseLocalAddress reads. It is
	// Options.Resolve, and net.DefaultResolver's lookup for a proxy given none.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// clusterRanges is Options.ClusterRanges, the broker cluster's own ranges, which localRule
	// refuses after its fixed rules.
	clusterRanges []netip.Prefix
	// hashBudget is the bytes of AWS body the proxy may hold at once to hash
	// (hashedBodyBudget, aws.go). One semaphore for the whole Server, because what it bounds
	// is the pod's memory, which every connection shares.
	hashBudget *semaphore.Weighted
	// dialAllow, keepValues and noKind are the three test-only settings of Options, each
	// refused on a production start of the broker (cmd/broker's
	// refuseAirlockTestSettingsInProduction).
	dialAllow  []string
	keepValues bool
	noKind     bool
	// azureAuthority is Options.AzureAuthority, and microsoftAuthority for a proxy given none.
	azureAuthority string

	// outer serves the bound listener: CONNECT and absolute-URL proxy requests. inner serves
	// what the outer one decrypted, through innerLn.
	outer   *http.Server
	inner   *http.Server
	innerLn *chanListener
	// upstream carries every request to a bound host's service, and the Azure token exchange.
	// Its TLSClientConfig trusts the host's roots as any client does, or Options.UpstreamRoots
	// when a caller named roots of its own. forward carries every plain-HTTP request the proxy
	// forwards, a socket of its own for each (New).
	upstream *http.Transport
	forward  *http.Transport
	// errorLog is the log.Logger both servers' ErrorLog and both ReverseProxies' (servePlain,
	// serveBound) are: the lines net/http's servers and httputil write of their own go through
	// log, so each is the airlock's own line, masked (maskedHandler) and cut (slogWriter), and an
	// error httputil logs can quote what a service sent, its trailers among it. net/http's
	// Transport has no ErrorLog: what it logs - what a service sent on an idle connection, which
	// it quotes - it writes with log.Printf to the standard logger, which cmd/broker masks and
	// cuts as a whole (SetDefaultLog), and which is the one place a line of the proxy's goes that
	// is not this logger.
	errorLog *log.Logger
	serveOne sync.Once

	// conns is the CONNECT target and the grant of each decrypted connection, which
	// ConnContext reads into the context of every request on it.
	conns sync.Map // net.Conn -> connInfo

	// open is every hijacked connection: each tunnel (as its two ends) and each bound
	// connection. The drain closes them, since http.Server.Shutdown does not.
	openMu sync.Mutex
	open   map[io.Closer]struct{}

	// values is each grant's real values, read once per grant on this pod and held until the
	// reread of one of its names drops it (DropName) or a minted value nears its expiry
	// (grantValues, mintAgainBefore).
	valuesMu sync.Mutex
	values   map[string]*grantValues

	// access is each access token this pod exchanged a grant's federated token for, by grant,
	// name and slot (azure.go). It is this pod's own: the placeholder that stands for one of
	// them is the whole grant's, and a pod holding none for a slot exchanges for itself.
	accessMu sync.Mutex
	access   map[accessKey]*accessHeld
}

// connInfo is what a decrypted connection carries into each request on it: the CONNECT target
// it was opened for, and the proxy password it was opened with, which each request reads the
// grant again with.
type connInfo struct {
	// target is the CONNECT authority, always with a port; host is its host alone, ASCII
	// lower-cased, which is the airlock.Request's ConnectHost, and port is that authority's
	// port as a number, which is its ConnectPort. A bound connection is opened on port 443
	// alone (the candidates of any other port are none), and the port travels here all the
	// same so that the decision reads the port the CONNECT named rather than taking it on
	// trust.
	target string
	host   string
	port   uint16
	// password is the proxy password this connection was opened with: a placeholder of the
	// grant. Every request on the connection - every stream of an HTTP/2 one - reads the grant
	// again with it (serveBound), so a grant that has ended, been revoked or had one of its
	// names withheld since the CONNECT stops at the next request rather than at the idle
	// limit. A call that runs on reads no request again, so its watcher reads with it instead
	// (watchCarried).
	password string
	// grant is what that password proved when the connection was opened. Nothing is decided
	// from it - each request is decided from the grant it read for itself - and what it is
	// here for is the line a request logs before that read answers.
	grant requests.AirlockConnection
	// cut is the connection under this one's TLS, which the cut of a joined connection
	// stops before it writes close_notify (cutJoined). It travels here so that the watcher
	// of a join reaches it as it reaches everything else it was opened with, rather than
	// sniffing a type out of the chain of wrappers under the decrypted connection.
	cut *cutConn
}

// connKey is the context key connInfo travels under.
type connKey struct{}

// decryptedKey is the context key the decrypted connection itself travels under. A request that
// is answered and done needs nothing of it - net/http owns the connection - and an upgrade does:
// once the two connections are joined, closing this one is the only way the grant behind it
// still reaches it (watchCarried, cutJoined).
type decryptedKey struct{}

// New builds the proxy. Nothing is bound and nothing is served until Serve.
func New(o Options) *Server {
	s := &Server{
		rules:         o.Rules,
		grants:        o.Grants,
		ca:            o.CA,
		idle:          o.Idle,
		watch:         o.Watch,
		log:           o.Log,
		answers:       o.Answers,
		dial:          o.Dial,
		now:           o.Now,
		resolve:       o.Resolve,
		clusterRanges: o.ClusterRanges,
		dialAllow:     o.DialAllow,
		keepValues:    o.KeepValues,
		noKind:        o.NoKind,
		innerLn:       newChanListener(),
		open:          map[io.Closer]struct{}{},
		values:        map[string]*grantValues{},
		access:        map[accessKey]*accessHeld{},
	}
	if o.HashBudget <= 0 {
		o.HashBudget = hashedBodyBudget
	}
	s.hashBudget = semaphore.NewWeighted(o.HashBudget)
	if s.log == nil {
		s.log = slog.Default()
	}
	// Every line this package writes - net/http's through slogWriter among them - is written
	// with each placeholder in its text masked (maskedHandler).
	s.log = slog.New(maskedHandler{s.log.Handler()})
	if s.resolve == nil {
		s.resolve = ResolveHost
	}
	if s.dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		s.dial = dialer.DialContext
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.watch <= 0 {
		s.watch = watchInterval
	}
	s.azureAuthority = o.AzureAuthority
	if s.azureAuthority == "" {
		s.azureAuthority = microsoftAuthority
	}
	s.upstream = &http.Transport{
		// The whole check-and-dial, so that a request to a bound host reaches the address
		// this proxy resolved and checked for the name and no other: net/http hands this the
		// host name, which dialChecked resolves once, refuses as a whole and then dials. The
		// TLS server name stays the host name, which is what the service's certificate is
		// read against.
		DialContext:         s.dialChecked,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.UpstreamRoots},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: upstreamIdleConns,
		IdleConnTimeout:     upstreamIdleTimeout,
	}
	// The transport an absolute-URL request is forwarded on, built here rather than cloned from
	// upstream so that nothing of upstream's can make it speak HTTP/2: dialled by dialForwarded,
	// which writes the line of each socket it opens, with upstream's TLS configuration, keeping
	// no connection for a later request, and speaking HTTP/1.1 alone. A kept connection would
	// carry the next request to its host with no dial and so with no line, a request of another
	// caller's among them, so each request opens a socket of its own and its line names its own
	// caller. HTTP/2 would undo that: with keep-alives off an h2 connection still carries every
	// request the transport hands it while it is open, and a request of another caller can be
	// handed one a first request dialled. TLSNextProto is an empty map, never nil, since net/http
	// negotiates h2 over TLS only when it is nil, and Protocols names HTTP/1 alone.
	var forwardProtocols http.Protocols
	forwardProtocols.SetHTTP1(true)
	s.forward = &http.Transport{
		DialContext:       s.dialForwarded,
		TLSClientConfig:   s.upstream.TLSClientConfig.Clone(),
		DisableKeepAlives: true,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
		Protocols:         &forwardProtocols,
	}
	// net/http writes a line of its own through ErrorLog: for example for a connection that fails
	// before a handler sees it, for a handler's panic it recovers, or for an accept that failed;
	// routing it through the broker's logger keeps it out of a bare stderr write.
	s.errorLog = log.New(slogWriter{log: s.log}, "", 0)
	s.outer = &http.Server{
		// Every answer a handler of this server writes is recorded (record): a CONNECT the
		// proxy takes over answers 200 on the connection itself. net/http's own answers, which
		// it writes before any handler runs (a 400 for a malformed request, a 431, a 417), are
		// not.
		Handler:           s.record(http.StatusOK, s.serveProxy),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       o.Idle,
		ErrorLog:          s.errorLog,
	}
	s.inner = &http.Server{
		// Every answer a handler of this server writes is recorded (record): an upgrade
		// ReverseProxy joins to its service answers 101 on the connection itself. net/http's
		// own answers, which it writes before any handler runs, are not: a 400 for a malformed
		// request, a 431, a 417, and the 400 "Client sent an HTTP request to an HTTPS server"
		// to plain HTTP on a connection the proxy decrypts.
		Handler:           s.record(http.StatusSwitchingProtocols, s.serveBound),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       o.Idle,
		ErrorLog:          s.errorLog,
		// The two protocols connectBound's TLS offers, named rather than left to net/http's
		// default, which a TLSNextProto map set beside it would narrow to HTTP/1 alone. The
		// one switch that still takes HTTP/2 away is GODEBUG=http2server=0, which net/http
		// reads whatever this says (onceSetNextProtoDefaults); with it, a client offering h2
		// alone has its connection closed once its handshake is done.
		Protocols: innerProtocols(),
		// How many streams one HTTP/2 connection may have open at once: RFC 9113's floor
		// (section 6.5.2), where net/http's default is 250. Each stream is a grant read of its
		// own and a stream to the service, and a gRPC client queues a call past the limit
		// rather than failing it.
		HTTP2: &http.HTTP2Config{MaxConcurrentStreams: maxStreams},
		// The CONNECT target and the grant of the connection this request arrived on, and the
		// connection itself. The request's own Host header - an HTTP/2 stream's :authority - is
		// checked against that target by airlock.Decide, so a request that names another host
		// is refused rather than carried; the connection is there for an upgrade, whose handler
		// holds it for as long as the two ends are joined (watchCarried). Every stream of an
		// HTTP/2 connection has this context under its own.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			info, _ := s.conns.Load(c)
			ctx = context.WithValue(ctx, connKey{}, info)
			return context.WithValue(ctx, decryptedKey{}, c)
		},
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				s.conns.Delete(c)
				s.forget(c)
			}
		},
	}
	return s
}

// maxStreams is the most streams one HTTP/2 connection of a bound host may have open at once.
const maxStreams = 100

// innerProtocols is what the inner server speaks: HTTP/1.1 and HTTP/2 over the TLS the proxy
// opened, and HTTP/2 in the clear never.
func innerProtocols() *http.Protocols {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	return &protocols
}

// Serve takes the proxy port's listener, and the inner server with it. It returns what
// http.Server.Serve returns, http.ErrServerClosed for a drained proxy among it.
func (s *Server) Serve(ln net.Listener) error {
	s.serveOne.Do(func() {
		go func() {
			// The inner server ends with the channel listener the drain closes, so its own
			// ErrServerClosed and net.ErrClosed are the drain and say nothing.
			_ = s.inner.Serve(s.innerLn)
		}()
	})
	return s.outer.Serve(ln)
}

// Shutdown drains the proxy inside ctx: both servers, and then every hijacked connection, which
// http.Server.Shutdown neither closes nor waits for. A command whose tunnel is open when the pod
// drains therefore loses it at the end of the shutdown rather than at the kubelet's kill.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.outer.Shutdown(ctx)
	s.innerLn.Close()
	if innerErr := s.inner.Shutdown(ctx); err == nil {
		err = innerErr
	}
	s.closeOpen()
	return err
}

// serveProxy is the whole of the outer server: a proxy request, a credential that proves a live
// grant, and then either a tunnel or a bound host.
func (s *Server) serveProxy(w http.ResponseWriter, r *http.Request) {
	// A request that is neither CONNECT nor an absolute-URL proxy request is somebody treating
	// this port as an ordinary web server. It serves nothing of its own, not even a health
	// endpoint: the API port is where the broker answers.
	if r.Method != http.MethodConnect && !r.URL.IsAbs() {
		s.refuse(w, r, call{host: requestHost(r)}, http.StatusBadRequest, codeNotAProxyRequest, "not a proxy request: this port takes CONNECT and absolute-URL proxy requests alone")
		return
	}
	placeholder, why := proxyPassword(r)
	if why != "" {
		s.refuseCredential(w, r, call{host: requestHost(r)}, codeProxyCredential, why)
		return
	}
	grant, err := s.grants.AirlockConnection(r.Context(), placeholder)
	if err != nil {
		status, code, why := s.grantRefusal("connection", err)
		switch status {
		case http.StatusProxyAuthRequired:
			// The broker's own words for why this credential opens nothing - an unknown or
			// ended grant, an enrollment whose lease lapsed, a grant every one of whose
			// names is withheld - and never the placeholder itself, which a command's own
			// proxy error line would otherwise print and keep.
			s.refuseCredential(w, r, call{host: requestHost(r)}, code, why)
		case http.StatusServiceUnavailable:
			// A read that did not happen, which grantRefusal has logged: a failure of the
			// broker's, and no refusal of the caller.
			fail(w, http.StatusServiceUnavailable, code, why)
		default:
			// The broker's own refusal of this grant, in its own words (grantRefusal).
			s.refuse(w, r, call{host: requestHost(r)}, status, code, why)
		}
		return
	}
	// Every dial below this point is made for this grant, and the lines it writes name it
	// (dialFields): the CONNECT's own check and its tunnel's dial run under this context, and a
	// plain-HTTP request's transport dials under it too.
	r = r.WithContext(withCaller(r.Context(), grant))
	if r.Method == http.MethodConnect {
		s.serveConnect(w, r, placeholder, grant)
		return
	}
	s.servePlain(w, r, grant)
}

// proxyPassword is the placeholder a proxy request authenticates with: the password of its
// Proxy-Authorization Basic credential. The user name is not read - a command puts anything
// there, and several of the measured clients put the secret's own name - and the second answer
// is why there is no password, which the caller answers 407.
func proxyPassword(r *http.Request) (string, string) {
	credential := r.Header.Get("Proxy-Authorization")
	if credential == "" {
		return "", "this port takes a Proxy-Authorization: Basic credential whose password is a placeholder of a live grant, and this request carries none"
	}
	scheme, payload, ok := strings.Cut(credential, " ")
	if !ok || !strings.EqualFold(scheme, "basic") {
		return "", "the Proxy-Authorization credential is not Basic"
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.Trim(payload, " \t"))
	if err != nil {
		return "", "the Basic credential is not base64"
	}
	_, password, _ := strings.Cut(string(decoded), ":")
	return password, ""
}

// refuseCredential answers 407 and asks for the credential again (refuse, with the challenge a
// client sends its credential again on). Nothing is tunnelled and nothing is dialled: a request
// that reaches here never left the broker.
func (s *Server) refuseCredential(w http.ResponseWriter, r *http.Request, c call, code, why string) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="airlock"`)
	s.refuse(w, r, c, http.StatusProxyAuthRequired, code, why)
}

// requestHost is the host a request of the outer server asks for, as a refusal's line names it
// before anything has read the request as a call: an absolute URL's host, and otherwise the
// request's Host, which is a CONNECT's authority, without its port. Both are ASCII lower-cased,
// as a bound connection's host is.
func requestHost(r *http.Request) string {
	if r.URL.IsAbs() {
		return fold.ASCII(r.URL.Hostname())
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	return fold.ASCII(host)
}

// grantUnreadable is what a command is told when a read of its grant did not answer at all. It
// names nothing of the read that failed - a Postgres connect error carries the database's user,
// its name and its address - and it says the call can be made again, because nothing in such a
// failure says the grant has ended.
const grantUnreadable = "the broker could not read this grant just now; make the call again"

// The codes of a grant read's answers (grantRefusal): the API port's own code where it answers
// the same refusal of a grant's release (api's grantValues), and one of this port's own for a
// refusal the API port names no code for and for a read that did not happen.
const (
	codeGrantNotLive     = "GRANT_NOT_LIVE"
	codeAirlockNoNames   = "AIRLOCK_NO_NAMES"
	codeGrantChain       = "GRANT_CHAIN_INVALID"
	codeSecretNotInStore = "SECRET_NOT_IN_STORE"
	codeMintFailed       = "MINT_FAILED"
	codeValueShape       = "AIRLOCK_VALUE_SHAPE"
	codeGrantRefused     = "GRANT_REFUSED"
	codeGrantUnreadable  = "GRANT_UNREADABLE"
)

// grantRefusal is the status, the code and the words one failed grant read is answered with,
// and it is the one rule for every read of a grant this package makes. Three answers, by what
// the read said:
//
//   - A grant that has ended, and a grant holding no name this session may have, are the read's
//     own answer about that grant, so they are 407 in the broker's own words, which is what
//     tells a command to ask for a new grant.
//   - A mint the secret's own minter could not even ask for - the upstream did not answer, or
//     answered a 5xx or a rate limit (secrets.ErrMintUpstream) - is a read that did not happen,
//     and it falls to the last answer below. It reaches here wrapped in ErrMintFailed, which is
//     a refusal of the package the line below asks, so it has to be read first: a re-mint of a
//     github-app, google-login or azure-login name behind the airlock fails this way whenever
//     its upstream has a bad minute, and answering that 403 would tell a command no retry
//     clears it and would close a WebSocket the grant still carries.
//   - Every other refusal the broker made about this grant - a chain that no longer verifies, a
//     secret the store no longer holds, a mint the secret's own minter refused, a value whose
//     shape the airlock cannot take apart - is 403 in those words. No retry clears one of them,
//     so telling a command to make the call again would be untrue, and none of them carries
//     anything of the store the read went to. Which errors those are is the read's own package
//     to say (requests.IsRefusal), since a sentinel added there would be missed by a list kept
//     here; the four named here have a code of their own, and any other is GRANT_REFUSED.
//   - Everything left is a read that did not happen - a database that did not answer, most of
//     them - and is 503 and grantUnreadable, with the error itself in the log, where it is
//     wanted: a failed read is no proof that a grant ended, and a database failover must not
//     send every agent in the middle of a call to ask for a grant it already holds, which on
//     the human tier is a person approving each one. Nor may the answer carry the read's own
//     words, which for a Postgres connect error are the database's user, name and address.
func (s *Server) grantRefusal(read string, err error) (int, string, string) {
	switch {
	case errors.Is(err, requests.ErrGrantNotLive):
		return http.StatusProxyAuthRequired, codeGrantNotLive, err.Error()
	case errors.Is(err, requests.ErrAirlockNoNames):
		return http.StatusProxyAuthRequired, codeAirlockNoNames, err.Error()
	case errors.Is(err, secrets.ErrMintUpstream):
		// The upstream, not the grant: this one falls through to the 503 below, ahead of the
		// refusal line, which would otherwise take it for one.
	case errors.Is(err, requests.ErrGrantChainInvalid):
		return http.StatusForbidden, codeGrantChain, err.Error()
	case errors.Is(err, requests.ErrSecretNotInStore):
		return http.StatusForbidden, codeSecretNotInStore, err.Error()
	case errors.Is(err, requests.ErrMintFailed):
		return http.StatusForbidden, codeMintFailed, err.Error()
	case errors.Is(err, requests.ErrAirlockValueShape):
		return http.StatusForbidden, codeValueShape, err.Error()
	case requests.IsRefusal(err):
		return http.StatusForbidden, codeGrantRefused, err.Error()
	}
	s.log.Error("airlock proxy: a grant could not be read", "read", read, "error", err)
	return http.StatusServiceUnavailable, codeGrantUnreadable, grantUnreadable
}

// serveConnect answers one CONNECT: a host one of the grant's names is bound to is decrypted
// and read, any other host is a tunnel. Both are refused before any dial when the host resolves
// somewhere this proxy never goes.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request, placeholder string, grant requests.AirlockConnection) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	// The authority, with its host ASCII lower-cased: it is the URL and the Host header of
	// every request this connection carries, and the one string the entry's hosts and the
	// request's own Host header are compared against. The reason quotes it lowercased too, as
	// every line names a host the command wrote: a command may write anything here, its own
	// placeholder among it.
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		s.refuse(w, r, call{grant: grant, host: requestHost(r)}, http.StatusBadRequest, codeConnectTarget, fold.ASCII(r.Host)+" is no host and port")
		return
	}
	// An address literal written as every address check reads it (netaddr.Host): the bound-host
	// check, the certificate and the dial below all read this one spelling of the host.
	host = netaddr.Host(fold.ASCII(host))
	// And the port as the number it names, in the one spelling of that number, before the check,
	// the bound-host decision, the dial and the lines read it (portNumber). The refusal names no
	// port: the text that is no port is the command's own, which may be anything at all.
	connectPort, ok := portNumber(port)
	if !ok {
		s.refuse(w, r, call{grant: grant, host: host}, http.StatusBadRequest, codeConnectTarget, "the CONNECT to "+host+" names no port from 1 to 65535")
		return
	}
	port = strconv.FormatUint(uint64(connectPort), 10)
	target = net.JoinHostPort(host, port)
	// The addresses this name has, read and checked once: the tunnel below dials these and
	// resolves nothing of its own, and a bound host's requests are checked and dialled the
	// same way by the transport they go out on (dialChecked).
	addrs, err := s.checkedAddresses(r.Context(), host, port)
	if err != nil {
		s.refuse(w, r, call{grant: grant, host: host}, http.StatusForbidden, codeAddressRefused, err.Error())
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		fail(w, http.StatusInternalServerError, codeNotHijackable, "this connection cannot be taken over")
		return
	}
	// Whether this CONNECT may be decrypted and read: the (name, slot) pairs of the grant
	// whose entry binds this host on the port the CONNECT named. One answer from the one
	// function that says what is bound (candidates), so a change to what is bound is made in
	// one place.
	//
	// An empty answer is a tunnel rather than a refusal, whether it is a host no name binds or
	// another port of a bound name: the airlock blocks nothing it does not bind (R3), the
	// proxy reads nothing on such a connection and puts nothing into it, and the command
	// reaches that service exactly as it reaches every host the rules say nothing about. The
	// port is in it because every name the decision compares - the CONNECT host, the TLS name
	// and the Host header - carries no port, so 443 is the only port an entry's hosts speak
	// about and another port of one name is another service.
	if len(s.candidates(grant, host, connectPort)) > 0 {
		s.connectBound(w, hijacker, placeholder, grant, target, host, connectPort)
		return
	}
	s.connectTunnel(w, r, hijacker, target, host, addrs, port)
}

// portNumber is the port a caller wrote as the number it names, and false for text that names no
// port: anything but decimal digits, 0, or a number past 16 bits. A command writes the port it
// asks for, and the dialer reads more than one spelling of one port - leading zeros, a service
// name such as "https", a sign - so a port the proxy did not read for itself could reach a bound
// host's 443 as a tunnel, or be written on a line as text no query for its number finds. The
// proxy reads it once, here, and writes it back as strconv.FormatUint spells the number before
// anything checks it, dials it or logs it.
func portNumber(port string) (uint16, bool) {
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return 0, false
	}
	return uint16(number), true
}

// connectBound answers a CONNECT to a host one of the grant's names is bound to, on port 443:
// the proxy opens the TLS itself, with a certificate of its own authority, and hands the
// decrypted connection to the inner server. What travels with the connection is the target and
// the password it was opened with: each request on it reads the grant again with that password
// and builds its own candidates from what that read answers (serveBound), so a grant that ends
// or a name that is withheld between two requests stops at the second one.
func (s *Server) connectBound(w http.ResponseWriter, hijacker http.Hijacker, placeholder string, grant requests.AirlockConnection, target, host string, port uint16) {
	client, err := hijack(hijacker)
	if err != nil {
		return
	}
	if _, err := client.Write(established(w)); err != nil {
		client.Close()
		return
	}
	cut := &cutConn{Conn: newIdleConn(client, s.idle)}
	decrypted := tls.Server(cut, &tls.Config{
		// http/1.1 first and h2 after it, and crypto/tls answers the first protocol of this
		// list the client offers too, so a client that offers both - net/http's own, curl,
		// git - speaks HTTP/1.1 here, and a client that offers h2 alone - every gRPC client -
		// speaks HTTP/2. One HTTP/2 connection carries many requests at once, each a stream
		// with its own :authority, which net/http puts in r.Host; each stream is one request
		// to serveBound, which reads the grant again for it and has airlock.Decide check its
		// Host against the CONNECT target, and what it forwards goes to that target whatever
		// the stream named. So the Host check covers every stream, as it covers every
		// request of an HTTP/1.1 connection.
		NextProtos: []string{"http/1.1", "h2"},
		// HTTP/2 over TLS needs 1.2 at least (RFC 9113, section 9.2). It is crypto/tls's own
		// default for a server, written here so the HTTP/2 this connection speaks never rests
		// on a default.
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			// The CONNECT target's host, not the ClientHello's name: a client that sends no
			// SNI still gets the certificate of the host it asked for, and a client whose SNI
			// differs from its CONNECT is refused by the decision rather than served a
			// certificate for the name it smuggled in.
			return s.ca.leaf(host)
		},
	})
	connection := net.Conn(decrypted)
	s.conns.Store(connection, connInfo{target: target, host: host, port: port, password: placeholder, grant: grant, cut: cut})
	s.track(connection)
	if !s.innerLn.hand(connection) {
		// The drain closed the listener between the hijack and here.
		s.conns.Delete(connection)
		s.forget(connection)
		connection.Close()
	}
}

// connectTunnel answers a CONNECT to every other host: bytes in both directions and nothing
// read. The dial comes before the hijack, so a service that cannot be reached is a 502 the
// command's own HTTP client reports, rather than a tunnel that opens and then dies. It dials
// the addresses serveConnect checked, in order, and resolves nothing of its own.
//
// The socket that opened writes one line (logTunnel) as soon as it is open, and a dial that
// failed writes one (logUnreachable): no request of a tunnel is read, so these are the whole of
// what the broker knows about where a command went through it. The line comes before the hijack
// and the 200, so a command that went away between its dial and its answer, whose tunnel never
// carries a byte, still leaves the line of the socket the proxy opened for it.
func (s *Server) connectTunnel(w http.ResponseWriter, r *http.Request, hijacker http.Hijacker, target, host string, addrs []netip.Addr, port string) {
	upstream, dialed, err := s.dialAddrs(r.Context(), "tcp", addrs, port)
	if err != nil {
		s.logUnreachable(r.Context(), host, port, addrs, err)
		fail(w, http.StatusBadGateway, codeUpstream, "dial "+target+": "+err.Error())
		return
	}
	s.logTunnel(r.Context(), host, port, dialed)
	client, err := hijack(hijacker)
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write(established(w)); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	// The idle limit on both sides, so that neither end of the tunnel outlives it. The
	// command's side alone would not: once that side has half-closed, the copy below sits in
	// a read of the upstream connection, which a service that answers nothing and closes
	// nothing never ends, and closing the command's side would not wake it. Both wrappers see
	// the same bytes at the same moment - every byte read from one end is written to the
	// other - so the window is the one window, counted twice.
	bounded := newIdleConn(client, s.idle)
	boundedUpstream := newIdleConn(upstream, s.idle)
	ends := bothEnds{client: bounded, upstream: boundedUpstream}
	s.track(ends)
	// The one end of this tunnel, whichever copy reaches it first: both connections closed and
	// the registry's entry dropped, so nothing of a tunnel outlives the copy that ended it.
	var closed sync.Once
	end := func() {
		closed.Do(func() {
			ends.Close()
			s.forget(ends)
		})
	}
	go func() {
		if _, err := io.Copy(boundedUpstream, bounded); err != nil {
			// The command's side is gone, or the idle limit closed it: there is nothing left
			// to carry in either direction.
			end()
			return
		}
		// A command that has said everything it has to say - git's upload-pack, a shell
		// pipeline - closes its half and waits for the answer, so the half-close is passed on
		// rather than turned into a close of the whole tunnel. What bounds the half-closed
		// state is the upstream's own idle limit above: the service has that long to send
		// something, and each byte it sends pushes the window out again.
		if closer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
			return
		}
		end()
	}()
	go func() {
		_, _ = io.Copy(bounded, boundedUpstream)
		end()
	}()
}

// servePlain answers an absolute-URL proxy request, which is plain HTTP: a command that asked
// for http:// rather than https://, or that sent an https URL to the proxy as it is rather than
// as a CONNECT.
func (s *Server) servePlain(w http.ResponseWriter, r *http.Request, grant requests.AirlockConnection) {
	host := netaddr.Host(fold.ASCII(r.URL.Hostname()))
	// The port as the number it names (portNumber), and the scheme's own for a URL that names
	// none, which is the port the transport dials for it.
	port := r.URL.Port()
	if port == "" {
		port = "80"
		if r.URL.Scheme == "https" {
			port = "443"
		}
	}
	number, ok := portNumber(port)
	if !ok {
		s.refuse(w, r, call{grant: grant, host: host}, http.StatusBadRequest, codeProxyTarget, "the request to "+host+" names no port from 1 to 65535")
		return
	}
	port = strconv.FormatUint(uint64(number), 10)
	// The authority the transport dials, in the one spelling every check and line here reads:
	// the host lowercased and without a zone, and the port as its number. The transport dials
	// the URL's own authority (dialForwarded), so a URL left as the command wrote it would be
	// dialled, and written on its lines, as that text - its own case, a zone, the port's leading
	// zeros - rather than as what was checked. It is set on a copy of the URL, since the request
	// this handler was handed shares its URL with the one the server read.
	forwarded := *r.URL
	forwarded.Host = net.JoinHostPort(host, port)
	r.URL = &forwarded
	// Whether any name of this grant may carry a secret to this host, from the one function
	// that says what is bound (candidates). It is asked about 443 because that is the only
	// port an entry's hosts speak about, while the refusal below is about no port at all: a
	// value may not cross in the clear whatever port it was asked for on.
	if len(s.candidates(grant, host, 443)) > 0 {
		// Plain HTTP to a bound host is refused rather than swapped into: the value would
		// cross the network in the clear, and no rule of the airlock says a secret may. The
		// command asks again over https, where the proxy's own certificate covers it.
		s.refuse(w, r, call{grant: grant, host: host}, http.StatusForbidden, codePlainHTTP, "refusing plain http to "+host+", a host behind the airlock: a secret of this grant may reach it only inside TLS, so ask for it over https")
		return
	}
	// The name's addresses, checked before anything is forwarded so that the refusal is a 403
	// of this proxy's rather than the transport's 502. The transport below checks and dials
	// the name again (dialForwarded), which is what makes the address it opens a socket to one
	// it checked itself.
	if _, err := s.checkedAddresses(r.Context(), host, port); err != nil {
		s.refuse(w, r, call{grant: grant, host: host}, http.StatusForbidden, codeAddressRefused, err.Error())
		return
	}
	// A host no name of this grant is bound to: the proxy forwards it and puts nothing into it,
	// exactly as the tunnel above carries what it cannot read, on a socket of its own whose line
	// is the tunnel's (Server.forward).
	proxy := &httputil.ReverseProxy{
		Transport:     s.forward,
		FlushInterval: -1,
		// httputil's own lines - a body copy that failed, which quotes what the service sent -
		// go through this package's log rather than the standard logger (Server.errorLog).
		ErrorLog: s.errorLog,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = r.URL
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
		},
		ErrorHandler: s.badGateway(host, r.URL.Path),
	}
	// The service's answer, which the proxy carries and does not write (record).
	answerOf(w).carry()
	proxy.ServeHTTP(w, r)
}

// upstreamMessage is the message of the line a call the proxy could not carry to its service logs
// (badGateway).
const upstreamMessage = "airlock proxy: upstream"

// badGateway answers a service that could not be reached or did not answer: a failure, which it
// logs and counts, and no refusal. The error is the transport's own and carries no header of the
// request, so nothing of the grant is in it, but it can quote what the service sent - a status
// line it could not read, which a bound service may write with the real value the swap sent it -
// so each run of the service's bytes it quotes with %q is written as peerBytesShown
// (hidePeerBytes), in the line and in the answer the command is given alike. The path, and on a
// plain request the host, are the caller's, and the error may quote the host, so the line passes
// answer.ClipFields as a call's line does, and stays one Cloud Logging entry whatever the caller
// sent.
func (s *Server) badGateway(host, path string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		reason := hidePeerBytes(err.Error())
		s.log.Warn(upstreamMessage, answer.ClipFields([]any{"host", host, "path", path, "error", reason})...)
		fail(w, http.StatusBadGateway, codeUpstream, host+": "+reason)
	}
}

// hijack takes the client's connection over from the outer server, with whatever the client
// wrote behind its CONNECT in front of it: a client that did not wait for the "200 Connection
// established" leaves its first bytes in net/http's own reader, where nothing below would see
// them.
func hijack(hijacker http.Hijacker) (net.Conn, error) {
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	if n := buffered.Reader.Buffered(); n > 0 {
		ahead, err := buffered.Reader.Peek(n)
		if err != nil {
			conn.Close()
			return nil, err
		}
		// Peek answers a window into net/http's own buffer, so the bytes are copied out of it
		// before that buffer is anybody else's.
		return &prefixConn{Conn: conn, rest: bytes.NewReader(slices.Clone(ahead))}, nil
	}
	return conn, nil
}

// netHTTPMessage is the message of the line net/http writes through the ErrorLog of either
// server (slogWriter): for example a connection that failed before a handler saw it, such as a
// TLS handshake on a bound connection that failed, an accept that failed, and a handler's panic,
// which net/http recovers and logs with the stack of the goroutine that panicked.
const netHTTPMessage = "airlock proxy: net/http"

// slogWriter is net/http's ErrorLog as one slog line each, under netHTTPMessage: the first line
// of net/http's text as its error, and the rest of the text, where there is any, as its stack.
//
// The first line can quote what the client sent: a handshake that failed on a bound connection is
// logged with crypto/tls's error, which for an ALPN list without http/1.1 quotes the client's
// whole list, up to about 64 KB of protocol names, each byte of which %q may write as four and
// the JSON handler then escapes again. So the error passes answer.ClipFields as a call's line
// does.
//
// The rest is the stack net/http writes under a handler's panic it recovered, after any further
// lines of the panic value's own text, and it is kept whole, with no cut of its own: neither
// server recovers a panic before net/http does (record), so this line is the one record of where
// the panic happened, and the handler's own frames come after the runtime's, past the first 256
// bytes of the text. The stack is the server's own text, not the client's - frames, file paths,
// tabs and newlines, which the JSON handler writes in one or two bytes each - and net/http bounds
// it at 64 KiB, so it writes at most 128 KiB, and the line stays one Cloud Logging entry whatever
// the client sent. No handler of the airlock panics with a value that holds a newline: a panic
// value that did, and quoted the client, would put that text in this field uncut. Every run of a
// peer's bytes net/http and httputil quote with %q - a client's ALPN list, a trailer line a
// service sent - is written as peerBytesShown first (hidePeerBytes), since a bound service is
// sent the real value the swap put in and the placeholder mask cannot see it.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	text, stack, stacked := strings.Cut(hidePeerBytes(strings.TrimSuffix(string(p), "\n")), "\n")
	fields := answer.ClipFields([]any{"error", text})
	if stacked {
		fields = append(fields, "stack", stack)
	}
	w.log.Warn(netHTTPMessage, fields...)
	return len(p), nil
}

// hostHeaderOf is the Host a request on target carries, and the Host the proxy sends upstream:
// the authority with the default HTTPS port left off, as every HTTP client writes it.
func hostHeaderOf(target string) string {
	return strings.TrimSuffix(target, ":443")
}

// packages/envoy/cmd/cipher/store_unreached_test.go
//
// A Secret Manager call that never reached Secret Manager: gRPC fails it Unavailable with text of
// its own, which names the address it dialed and the system's reason. Its line says the call did
// not reach the store, and why by a fixed class, and keeps the one x509 phrase gax reads to stop
// retrying.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/status"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/testport"
)

// unknownAuthority is crypto/x509's words for a certificate no root this machine trusts signed,
// which gax reads to stop retrying a call.
const unknownAuthority = "x509: certificate signed by unknown authority"

// unreachedTarget is a Secret Manager address no call reaches, with the dial options that make it
// so, and the texts of it no line may print.
type unreachedTarget struct {
	target string
	opts   []grpc.DialOption
	hidden []string
}

// tlsUnreached is a TLS listener that offers h2, as gRPC asks, and whose certificate no root of
// this machine's signed, so the handshake fails with unknownAuthority.
func tlsUnreached(t *testing.T) unreachedTarget {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return unreachedTarget{addr, []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))}, []string{addr}}
}

// refusedUnreached is a loopback port that refuses every dial for the whole test.
func refusedUnreached(t *testing.T) unreachedTarget {
	addr := testport.Refused(t)
	return unreachedTarget{addr, []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, []string{addr}}
}

// unresolvedUnreached is a host whose lookup finds no such host: the dial fails as net's lookup
// does, with no network asked, as the passthrough resolver's dial of a real name fails.
func unresolvedUnreached() unreachedTarget {
	const host = "secretmanager.unreached.invalid"
	dial := func(context.Context, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return unreachedTarget{"passthrough:///" + host + ":443", []grpc.DialOption{grpc.WithContextDialer(dial), grpc.WithTransportCredentials(insecure.NewCredentials())}, []string{host}}
}

// dialFailing is an address whose every dial fails with err, with no network asked: gRPC words
// the failure "connection error: desc = ..." around err's text.
func dialFailing(err error) func(*testing.T) unreachedTarget {
	return func(*testing.T) unreachedTarget {
		dial := func(context.Context, string) (net.Conn, error) { return nil, err }
		return unreachedTarget{"passthrough:///secretmanager.unreached.example:443", []grpc.DialOption{grpc.WithContextDialer(dial), grpc.WithTransportCredentials(insecure.NewCredentials())}, []string{"unreached.example"}}
	}
}

// baseBalancerName is a balancer built on grpc's balancer/base, which words a connection it could
// not make "last connection error: ..." before gRPC's own words for it, as base's users and
// grpclb do; Secret Manager's own dial uses pick_first, which does not.
const baseBalancerName = "agent_secrets_test_base"

func init() {
	balancer.Register(base.NewBalancerBuilder(baseBalancerName, firstReady{}, base.Config{}))
}

// firstReady picks a ready connection, the one the base balancer gives it.
type firstReady struct{}

func (firstReady) Build(info base.PickerBuildInfo) balancer.Picker {
	for sc := range info.ReadySCs {
		return pickOnly{sc}
	}
	return base.NewErrPicker(balancer.ErrNoSubConnAvailable)
}

// pickOnly picks sc for every call.
type pickOnly struct{ sc balancer.SubConn }

func (p pickOnly) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	return balancer.PickResult{SubConn: p.sc}, nil
}

// baseBalancedRefused is a refused loopback port dialed under baseBalancerName.
func baseBalancedRefused(t *testing.T) unreachedTarget {
	addr := testport.Refused(t)
	return unreachedTarget{addr, []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"` + baseBalancerName + `":{}}]}`)}, []string{addr}}
}

// zeroAddressesUnreached is a name the resolver answers with no address, as gRPC's dns resolver
// answers a name that does not exist.
func zeroAddressesUnreached() unreachedTarget {
	r := manual.NewBuilderWithScheme("unreached")
	r.InitialState(resolver.State{})
	return unreachedTarget{r.Scheme() + ":///secretmanager.unreached.example", []grpc.DialOption{grpc.WithResolvers(r), grpc.WithTransportCredentials(insecure.NewCredentials())}, []string{"unreached.example"}}
}

// TestASecretManagerCallThatDidNotReachItSaysSo: a list whose ListSecrets never reached Secret
// Manager - a TLS handshake no root here trusts, a refused connection, a name with no such host, a
// name the resolver gave no address, a dial that timed out, a dial that failed for another reason -
// prints that the call did not reach Secret Manager and the class of the failure, whatever words
// a balancer puts before gRPC's, in redact.Class's
// words for the same failure of any request, never gRPC's text, its address and the system's reason
// among it, and never "answered". A server that answers Unavailable in gRPC's own words for a
// failed connection reached nothing it can claim: it prints as Secret Manager's answer, its message
// not printed. Control: Secret Manager's own Unavailable, with a made-up secret in its message,
// prints as its answer.
func TestASecretManagerCallThatDidNotReachItSaysSo(t *testing.T) {
	planted := "hunter2pw-" + "unavailable-" + "6d2c"
	forbidden := []string{planted, "connection error", "transport:", "desc =", "127.0.0.1", "resolver error", "zero addresses"}
	for _, row := range []struct {
		name string
		to   func(t *testing.T) unreachedTarget
		// answers is the message of the Unavailable a local server answers, where to is nil.
		answers string
		says    string
	}{
		{"a TLS handshake no root here trusts", tlsUnreached, "",
			"Secret Manager ListSecrets failed: it did not reach Secret Manager (" + redact.ClassTLS + ": " + unknownAuthority + ")"},
		{"a refused connection", refusedUnreached, "",
			"Secret Manager ListSecrets failed: it did not reach Secret Manager (" + redact.ClassRefused + ")"},
		{"a name with no such host", func(*testing.T) unreachedTarget { return unresolvedUnreached() }, "",
			"Secret Manager ListSecrets failed: it did not reach Secret Manager (" + redact.ClassNameResolution + ")"},
		{"a name the resolver gave no address", func(*testing.T) unreachedTarget { return zeroAddressesUnreached() }, "",
			"Secret Manager ListSecrets failed: it did not reach Secret Manager (" + redact.ClassNameResolution + ")"},
		{"a refused connection under a balancer that words it first", baseBalancedRefused, "",
			"Secret Manager ListSecrets failed: it did not reach Secret Manager (" + redact.ClassRefused + ")"},
		{"a dial that timed out", dialFailing(&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}), "",
			"Secret Manager ListSecrets failed: it did not reach Secret Manager (" + redact.ClassTimedOut + ")"},
		{"a dial that failed for another reason", dialFailing(errors.New("socket: too many open files; echo: " + planted)), "",
			"Secret Manager ListSecrets failed: it did not reach Secret Manager (" + redact.ClassRequestFailed + ")"},
		{"a server answering Unavailable in gRPC's own words for a failed connection", nil,
			`connection error: desc = "transport: Error while dialing: dial tcp 10.0.0.1:443: connect: connection refused"; echo: ` + planted,
			"Secret Manager ListSecrets answered Unavailable (its message is not printed)"},
		{"control: Secret Manager's own Unavailable", nil, "the service is busy; echo: " + planted,
			"Secret Manager ListSecrets answered Unavailable (its message is not printed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake := newFakeSecretManager()
			var to unreachedTarget
			if row.to != nil {
				to = row.to(t)
			} else {
				lis, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server := grpc.NewServer(grpc.UnaryInterceptor(func(context.Context, any, *grpc.UnaryServerInfo, grpc.UnaryHandler) (any, error) {
					return nil, status.Error(codes.Unavailable, row.answers)
				}))
				secretmanagerpb.RegisterSecretManagerServiceServer(server, fake)
				go func() { _ = server.Serve(lis) }()
				t.Cleanup(server.Stop)
				to = unreachedTarget{lis.Addr().String(), []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, []string{"10.0.0.1", "did not reach"}}
			}
			useGCPDialTo(t, to.target, to.opts...)
			useNoADC(t)
			useNoGcloud(t)
			useLentGCPAccessToken(t, "lent-store-token", "ada@example.com")
			startSecretBrokerWith(t, gcpTestSettings(), gcpServedBy(fake, gcpTestSettings()))
			_, stderr, code := runSecret("list")
			if code != 1 {
				t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
			}
			wantNamedNotPrinted(t, "stderr", stderr, []string{row.says}, append(forbidden, to.hidden...)...)
		})
	}
}

// TestGaxStillStopsRetryingACertificateNoRootHereTrusts: AccessSecretVersion, which gax retries on
// Unavailable, through the interceptor every Secret Manager call goes through, is tried once where
// the TLS handshake failed with unknownAuthority, since gax reads that phrase in the error the
// interceptor words. Control: a refused connection, also Unavailable, is tried again. No row reads
// the clock: the call's deadline is a minute, far past gax's first backoff, and a second attempt
// ends the call (its context is cancelled once that attempt returns), so a retry is counted, not
// timed, and a row ends as soon as it has its answer.
func TestGaxStillStopsRetryingACertificateNoRootHereTrusts(t *testing.T) {
	attempts := func(t *testing.T, to unreachedTarget) (int32, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var tries atomic.Int32
		count := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			n := tries.Add(1)
			err := invoker(ctx, method, req, reply, cc, opts...)
			if n >= 2 {
				cancel()
			}
			return err
		}
		conn, err := grpc.NewClient(to.target, append(to.opts, grpc.WithChainUnaryInterceptor(count, (&gcpBackend{}).storeInterceptor))...)
		if err != nil {
			t.Fatal(err)
		}
		client, err := secretmanager.NewClient(ctx, option.WithGRPCConn(conn))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		_, err = client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: "projects/p/secrets/s/versions/latest"})
		return tries.Load(), err
	}
	t.Run("a TLS handshake no root here trusts", func(t *testing.T) {
		if tries, err := attempts(t, tlsUnreached(t)); tries != 1 || err == nil || !strings.Contains(err.Error(), unknownAuthority) {
			t.Fatalf("tried %d times, err %v; want once, with %q in the error", tries, err, unknownAuthority)
		}
	})
	t.Run("control: a refused connection", func(t *testing.T) {
		if tries, err := attempts(t, refusedUnreached(t)); tries < 2 {
			t.Fatalf("tried %d times, err %v; want gax to try the call again", tries, err)
		}
	})
}

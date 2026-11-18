package sandbox

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A request that is not of the code under test's shape, as another process of the machine may
// send to a reused loopback port, is answered 404 and recorded as nothing by this package's
// stand-ins, while the code's own request is still recorded, so each test's assertion on the
// record still fails on the code's own defect.
func TestAStandInRecordsOnlyTheCodesOwnRequests(t *testing.T) {
	t.Run("the cache's /healthz", func(t *testing.T) {
		server, credentials := healthServer(t, cacheRepo.String())
		send := func(method, path, authorization string) int {
			t.Helper()
			request, err := http.NewRequestWithContext(testwait.Context(t), method, server.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			_ = response.Body.Close()
			return response.StatusCode
		}
		for _, stranger := range []struct{ method, path string }{
			{http.MethodGet, "/computeMetadata/v1/instance/service-accounts/default/identity"},
			{http.MethodPost, "/healthz"},
			{http.MethodGet, "/tar"},
		} {
			if status := send(stranger.method, stranger.path, "Bearer another-process"); status != http.StatusNotFound {
				t.Fatalf("%s %s from another process: status %d, want 404", stranger.method, stranger.path, status)
			}
		}
		if got := credentials(); len(got) != 0 {
			t.Fatalf("the health stand-in recorded %q from other processes, want nothing", got)
		}
		// A health read is recorded with the credential it carried, so a read that carried the
		// cache's token still fails TestTheHealthReadAsksThePodAddressWithNoCredential.
		for _, authorization := range []string{"", "Bearer " + boundaryCacheToken} {
			if status := send(http.MethodGet, "/healthz", authorization); status != http.StatusOK {
				t.Fatalf("a health read: status %d, want 200", status)
			}
		}
		if got, want := credentials(), []string{"", "Bearer " + boundaryCacheToken}; !slices.Equal(got, want) {
			t.Fatalf("the health stand-in recorded %q, want the two health reads %q", got, want)
		}
	})

	t.Run("the TLS-terminating proxy", func(t *testing.T) {
		ca, caKey, _ := newCertificateAuthority(t)
		var connects atomic.Int32
		proxy := tlsTerminatingProxy(t, leafCertificate(t, ca, caKey), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}, func() { connects.Add(1) })
		send := func(method, target string) int {
			t.Helper()
			conn, err := net.Dial("tcp", proxy.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if deadline, bounded := testwait.Context(t).Deadline(); bounded {
				if err := conn.SetDeadline(deadline); err != nil {
					t.Fatal(err)
				}
			}
			line := method + " " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
			if method != http.MethodConnect {
				line = method + " " + target + " HTTP/1.1\r\nHost: metadata\r\n\r\n"
			}
			if _, err := io.WriteString(conn, line); err != nil {
				t.Fatal(err)
			}
			request := &http.Request{Method: method}
			response, err := http.ReadResponse(bufio.NewReader(conn), request)
			if err != nil {
				t.Fatalf("%s %s: %v", method, target, err)
			}
			_ = response.Body.Close()
			return response.StatusCode
		}
		for _, stranger := range []struct{ method, target string }{
			{http.MethodGet, "/computeMetadata/v1/instance/service-accounts/default/identity"},
			{http.MethodConnect, "169.254.169.254:80"},
			{http.MethodConnect, "example.com:443"},
		} {
			if status := send(stranger.method, stranger.target); status != http.StatusNotFound {
				t.Fatalf("%s %s from another process: status %d, want 404", stranger.method, stranger.target, status)
			}
		}
		if n := connects.Load(); n != 0 {
			t.Fatalf("the proxy counted %d CONNECTs of other processes, want none", n)
		}
		// git's CONNECT to github.com is still tunnelled and counted, so a pod that still heard
		// from the planted proxy still fails its row.
		if status := send(http.MethodConnect, "github.com:443"); status != http.StatusOK {
			t.Fatalf("CONNECT github.com:443: status %d, want 200", status)
		}
		if n := connects.Load(); n != 1 {
			t.Fatalf("the proxy counted %d CONNECTs after git's, want that one", n)
		}
	})
}

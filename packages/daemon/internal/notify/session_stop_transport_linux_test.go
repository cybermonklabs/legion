package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/stalledconnect"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// stopBudget is the clean stop's budget in production (CleanSessionStopBudget in
// internal/daemon), which these tests give the stop's read.
const stopBudget = 5 * time.Second

// A limit set on the shared transport, Go's http.DefaultTransport in production, changes nothing
// for the clean stop: stopTransport clones that transport and removes each fixed limit and each
// function that could carry one, so a read whose connect, TLS handshake or HTTP/2 answer never
// comes runs until its own context ends, and ends with that context's error. Each row gives a
// clone of Go's transport a one-minute response-header timeout and one limit, and stalls the step
// that limit bounds:
//   - a connect limit of 2 s, 4.9 s or 5 s, to a listener whose accept queue is full
//     (stalledconnect.Listen);
//   - a TLS-handshake limit of 2 s, 4.9 s or 5 s, to a listener that accepts and sends nothing
//     (heldTLSListener);
//   - a TLS dial hook, DialTLSContext or DialTLS, whose dialer has a 2 s limit, to that listener:
//     Go uses either in place of DialContext for an https request, so a hook the clone kept would
//     end the read after 2 s;
//   - an HTTP/2 health check, a ping after 1 s with no frame and the connection closed 1 s after
//     an unanswered ping, to a listener that completes TLS with HTTP/2 and then answers nothing
//     (heldH2Listener), which a clone that kept it would close after about 2 s.
//
// The row makes the stop's transport from that clone, checks it (wantNoFixedLimit), and sends
// the stop's read with the daemon's budget (stopBudget). A limit the stop's transport kept would
// end the read in a row under the budget after less than its context, with the transport's
// error, and race the context in a row at it. The clock is read before the context starts, so
// time the machine spends between the two counts toward the read and a correct stop never reads
// as early. The stop's transport is a clone, so the shared one keeps every limit it had, which
// each row checks after the read: fixedLimits still names on it each limit it named before (and
// may name more: cloning it sets up Go's own HTTP/2 switch, TLSNextProto, on it). The rows change
// no transport another test uses, so they run side by side.
func TestTheCleanStopsTransportKeepsNoLimitOfTheSharedOne(t *testing.T) {
	type row struct {
		name string
		// stall sets the row's limit on shared and returns the URL of a listener that stalls the
		// step the limit bounds.
		stall func(t *testing.T, shared *http.Transport) string
	}
	var rows []row
	for _, limit := range []time.Duration{2 * time.Second, 4900 * time.Millisecond, stopBudget} {
		rows = append(rows,
			row{"connect limited to " + limit.String() + " on the shared transport", func(t *testing.T, shared *http.Transport) string {
				shared.DialContext = (&net.Dialer{Timeout: limit, KeepAlive: 30 * time.Second}).DialContext
				return "http://" + stalledconnect.Listen(t)
			}},
			row{"TLS handshake limited to " + limit.String() + " on the shared transport", func(t *testing.T, shared *http.Transport) string {
				shared.TLSHandshakeTimeout = limit
				return "https://" + heldTLSListener(t)
			}})
	}
	hooked := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 2 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12}}
	rows = append(rows,
		row{"a DialTLSContext hook limited to 2s on the shared transport", func(t *testing.T, shared *http.Transport) string {
			shared.DialTLSContext = hooked.DialContext
			return "https://" + heldTLSListener(t)
		}},
		row{"a DialTLS hook limited to 2s on the shared transport", func(t *testing.T, shared *http.Transport) string {
			shared.DialTLS = func(network, address string) (net.Conn, error) {
				return hooked.DialContext(context.Background(), network, address)
			}
			return "https://" + heldTLSListener(t)
		}},
		row{"an HTTP/2 health check of 1s on the shared transport", func(t *testing.T, shared *http.Transport) string {
			address, roots := heldH2Listener(t)
			shared.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			shared.HTTP2 = &http.HTTP2Config{SendPingTimeout: time.Second, PingTimeout: time.Second}
			return "https://" + address
		}})
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			shared := http.DefaultTransport.(*http.Transport).Clone()
			shared.ResponseHeaderTimeout = time.Minute
			p := New(row.stall(t, shared), "envoy-bearer")
			limits := fixedLimits(shared)
			p.stops.Transport = stopTransport(shared)
			wantNoFixedLimit(t, p.stops.Transport)
			began := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), stopBudget)
			defer cancel()
			_, err := p.ReadSessionRegistration(ctx, "ses_dead")
			took := time.Since(began)
			if took < stopBudget || err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
				t.Fatalf("the clean stop's read, with %s, ended after %s with %v, want it ended at its own context's %s deadline",
					row.name, took, err, stopBudget)
			}
			after := fixedLimits(shared)
			if kept := !slices.ContainsFunc(limits, func(limit string) bool { return !slices.Contains(after, limit) }); len(limits) == 0 || !kept {
				t.Fatalf("the shared transport had %q before the stop's transport was made from it and %q after the stop's read, want each of the first still there, and not none: every other request keeps its limits",
					limits, after)
			}
		})
	}
}

// heldTLSListener listens on loopback, accepts each connection and sends nothing on it until t
// ends, so a TLS handshake to it never completes; it returns the address.
func heldTLSListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	return listener.Addr().String()
}

// heldH2Listener listens on loopback with TLS, offering HTTP/2 alone, and on each connection
// completes the handshake and then reads all it is sent and writes nothing, until t ends: an
// HTTP/2 request to it is never answered, and no frame of the server's ever arrives. It returns
// the address and the roots that trust its certificate, and fails t, once t's own work is done,
// if a connection negotiated anything but HTTP/2, so a row on it tests an HTTP/2 connection.
func heldH2Listener(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	certs := httptest.NewUnstartedServer(nil)
	certs.StartTLS()
	t.Cleanup(certs.Close)
	roots := x509.NewCertPool()
	roots.AddCert(certs.Certificate())
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certs.TLS.Certificates, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var held []net.Conn
	var negotiated []string
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
		if len(negotiated) == 0 || slices.ContainsFunc(negotiated, func(protocol string) bool { return protocol != "h2" }) {
			t.Errorf("the held HTTP/2 listener's connections negotiated %q, want h2 on at least one and on every one: the row tested no HTTP/2 connection", negotiated)
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
			go func() {
				tlsConn := conn.(*tls.Conn)
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				mu.Lock()
				negotiated = append(negotiated, tlsConn.ConnectionState().NegotiatedProtocol)
				mu.Unlock()
				_, _ = io.Copy(io.Discard, tlsConn)
			}()
		}
	}()
	return listener.Addr().String(), roots
}

// The connect or TLS handshake a clean stop's read starts ends with the read, not after it. Go's
// transport dials on a context it detaches from the request, so the read's context ends the read
// and leaves the dial running, and the stop's transport has no connect or TLS-handshake limit that
// would end it (stopTransport). So a read that fails ends the dial itself (stopFailed). Each row
// sends the read, through the production client (New), with the daemon's budget, to a peer that
// never completes the step: a connect to a full accept queue, or a TLS handshake to a listener that
// sends nothing. The row wraps the stop's own dialer (watchDial), so it sees when the connect
// returns or the connection it made is closed, and once the read has returned it waits for that,
// bounded by the test binary's deadline (testwait.Bounded): a dial that outlives the read fails by
// name, and one that ends with it passes however late the machine runs.
func TestTheCleanStopsDialEndsWithItsFailedRead(t *testing.T) {
	for _, step := range []string{"connect", "TLS handshake"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			target := "http://" + stalledconnect.Listen(t)
			if step == "TLS handshake" {
				target = "https://" + heldTLSListener(t)
			}
			p := New(target, "envoy-bearer")
			dialed, dialEnded := watchDial(p.stops.Transport.(*http.Transport))
			ctx, cancel := context.WithTimeout(context.Background(), stopBudget)
			defer cancel()
			_, err := p.ReadSessionRegistration(ctx, "ses_dead")
			returned := time.Now()
			if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
				t.Fatalf("the clean stop's read, its %s stalled, ended with %v, want its context's deadline", step, err)
			}
			select {
			case <-dialed:
			default:
				t.Fatalf("the clean stop's read ended with %v and dialed nothing, want it to have begun the %s it stalled on", err, step)
			}
			select {
			case <-dialEnded:
			case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
				t.Fatalf("the %s the clean stop's read started was still in progress %s after the read had returned with %v: nothing ended the dial the stop's transport detached from the read",
					step, time.Since(returned), err)
			}
		})
	}
}

// watchDial wraps transport's dialer, so a test sees the dial of the one request it sends:
// dialed is closed when the connect begins, and ended when the connect returns an error or the
// connection it made is closed.
func watchDial(transport *http.Transport) (dialed, ended <-chan struct{}) {
	begun, done := make(chan struct{}), make(chan struct{})
	var beginOnce, endOnce sync.Once
	end := func() { endOnce.Do(func() { close(done) }) }
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		beginOnce.Do(func() { close(begun) })
		conn, err := dial(ctx, network, address)
		if err != nil {
			end()
			return nil, err
		}
		return closeWatched{Conn: conn, closed: end}, nil
	}
	return begun, done
}

// closeWatched is a connection that tells closed when it is closed.
type closeWatched struct {
	net.Conn
	closed func()
}

func (c closeWatched) Close() error {
	c.closed()
	return c.Conn.Close()
}

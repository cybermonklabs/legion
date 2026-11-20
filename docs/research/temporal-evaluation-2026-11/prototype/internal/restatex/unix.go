package restatex

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// unixScheme starts an address that names a unix socket, `unix:<path>`, rather than a
// loopback HOST:PORT. A server the driver starts serves its APIs on unix sockets alone.
const unixScheme = "unix:"

// HTTPClient is a client for the API at address, and the base URL its requests are made
// against. An address `unix:<path>` is the unix socket at path; any other is a loopback
// HOST:PORT.
func HTTPClient(address string, timeout time.Duration) (*http.Client, string) {
	path, ok := strings.CutPrefix(address, unixScheme)
	if !ok {
		return &http.Client{Timeout: timeout}, "http://" + address
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialUnix(ctx, path)
	}}
	// The host names nothing: every request of this client goes to the one socket.
	return &http.Client{Timeout: timeout, Transport: transport}, "http://restate"
}

// dialUnix connects to the unix socket at path through the directory that holds it, as
// /proc/self/fd/<fd>/<name>: the path of a socket under an experiment's directory can be
// longer than the 108 bytes a unix socket address holds, and that name is short. It needs
// Linux's /proc, as the rest of the harness does.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open the directory of the socket %s: %w", path, err)
	}
	defer dir.Close()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(path)))
	if err != nil {
		return nil, fmt.Errorf("dial the socket %s: %w", path, err)
	}
	return conn, nil
}

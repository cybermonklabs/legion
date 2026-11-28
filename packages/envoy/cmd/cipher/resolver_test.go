// packages/envoy/cmd/cipher/resolver_test.go
//
// The test's resolver: a row owns the answer for each host name its requests look up, and for each
// address it names where nothing must answer, so no row takes its result from the machine's
// resolver or from what listens on the machine. useResolver sets it in this process, and
// runResolved runs agent-secrets as a child process with it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"syscall"
	"testing"
)

// noSuchHost is the answer of the test's resolver for a name that has no address: the dial fails
// as the dialer fails when its lookup finds no such host, and nothing asks the network.
const noSuchHost = ""

// connectionRefused is the answer of the test's resolver for an address ip:port where nothing
// listens: the dial fails as a connect to a closed port fails (syscall.ECONNREFUSED), and nothing
// asks the network.
const connectionRefused = "connection refused"

// resolvedHostsVar holds, as JSON, the hosts that a child process of runResolved looks up its
// names in (TestAgentSecretsResolvedHelper).
const resolvedHostsVar = "AGENT_SECRETS_TEST_RESOLVED_HOSTS"

// useResolver makes hosts the resolver of every request that this process sends through
// http.DefaultTransport for the rest of t. Every broker call goes through it, because the client
// of newClient sets no transport of its own. A key of hosts is a host name or an address ip:port.
// An address in hosts is answered first: by connectionRefused, or by an address ip:port to dial in
// its place. A name in hosts dials the address that it maps to (with the port of the request
// where the address names none), or fails as a name with no such host where it maps to
// noSuchHost. A name that is not in hosts fails with an error that names it, and an IP address
// that is not in hosts dials as it is. No name goes to the machine's resolver, and no request goes
// through a proxy that the environment names, because the proxy would look up the name in place
// of hosts.
func useResolver(t *testing.T, hosts map[string]string) {
	t.Helper()
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is a %T, not the *http.Transport that useResolver dials through", http.DefaultTransport)
	}
	own := base.Clone()
	dial := own.DialContext
	own.Proxy = nil
	own.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		to, owned := hosts[address]
		if !owned {
			if _, err := netip.ParseAddr(host); err == nil {
				return dial(ctx, network, address)
			}
			to, owned = hosts[host]
		}
		switch {
		case !owned:
			return nil, fmt.Errorf("the test's resolver has no answer for %s", host)
		case to == noSuchHost:
			return nil, &net.OpError{Op: "dial", Net: network, Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
		case to == connectionRefused:
			refused := &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
			if at, err := netip.ParseAddrPort(address); err == nil {
				refused.Addr = net.TCPAddrFromAddrPort(at)
			}
			return nil, refused
		}
		if _, err := netip.ParseAddrPort(to); err == nil {
			return dial(ctx, network, to)
		}
		return dial(ctx, network, net.JoinHostPort(to, port))
	}
	http.DefaultTransport = own
	t.Cleanup(func() {
		http.DefaultTransport = base
		own.CloseIdleConnections()
	})
}

// runResolved is runAgentSecrets with the test's resolver: it runs this test binary as
// agent-secrets (TestAgentSecretsResolvedHelper), which looks up each host name in hosts
// (useResolver) and never in the machine's resolver. The child's TMPDIR is a directory of the
// test's own: the child exits from inside its test, so cmdtest.Run, which TestMain runs it
// through, never removes the directory of builds it makes under TMPDIR, and that directory goes
// when the test ends.
func runResolved(t *testing.T, hosts map[string]string, broker, keyDir string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(hosts)
	if err != nil {
		t.Fatal(err)
	}
	env := append([]string{resolvedHostsVar + "=" + string(encoded), "TMPDIR=" + t.TempDir()}, extraEnv...)
	return runAgentSecrets(t, self, broker, keyDir, env, append([]string{"-test.run=^TestAgentSecretsResolvedHelper$", "--"}, args...)...)
}

// TestAgentSecretsResolvedHelper is agent-secrets in a child process of runResolved: it runs run
// on the arguments after "--", with the hosts in resolvedHostsVar as its resolver (useResolver),
// and exits with the code of run. The exec form replaces this process with the command, as it
// replaces agent-secrets. The built binary's main adds only os.Exit to run; the one other
// difference is TestMain's launchRenewer, which starts no sign-in renewer, and a row that
// runResolved runs holds no sign-in to renew (hostIsolation).
func TestAgentSecretsResolvedHelper(t *testing.T) {
	// Not parallel: it signals the test process or changes its signal handling (main.run, main.cmdLogin, signal.NotifyContext at login.go:48); it replaces a value of the whole process (main.useResolver, http.DefaultTransport set at resolver_test.go:83); it sets a package variable the package's other tests read (main.run, main.loadClientConfig, clientConfig set at config.go:98).
	encoded := os.Getenv(resolvedHostsVar)
	if encoded == "" {
		t.Skip("subprocess helper")
	}
	var hosts map[string]string
	if err := json.Unmarshal([]byte(encoded), &hosts); err != nil {
		t.Fatalf("%s holds no hosts: %v", resolvedHostsVar, err)
	}
	useResolver(t, hosts)
	os.Exit(run(flag.Args(), os.Stdout, os.Stderr))
}

package launcher

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// newMetadataFleet starts a microVM issue pod's launcher told to serve the endpoint at address,
// with one role, the tester, over a process environment whose two variables hold carried
// (holdMetadataVariables).
func newMetadataFleet(t *testing.T, carried, address string) *fleet {
	t.Helper()
	holdMetadataVariables(t, carried)
	return newFleetOf(t, Config{
		StopGrace: 2 * time.Second, MicroVM: true,
		Metadata: &MetadataConfig{WorkloadPool: testWorkloadPool, IdentityProvider: testIdentityProvider, address: address},
	}, "tester")
}

// servedMetadataAddress is the address the launcher named in its own environment once it listens,
// failing the test unless it is a port the listener bound on the loopback.
func servedMetadataAddress(t *testing.T) string {
	t.Helper()
	address := os.Getenv(MetadataHostVariable)
	host, port, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" || port == "" || port == "0" {
		t.Fatalf("the launcher's own %s=%q, want the loopback port its endpoint bound", MetadataHostVariable, address)
	}
	return address
}

// takeAPort listens on a loopback port of its own for the rest of the test, as another process of
// the container could on the endpoint's, and answers its address.
func takeAPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

// toldMetadata starts generation of the tester with env as its start command's own Env, answers
// what the endpoint's two variables held in its environment ("unset" for one it does not hold),
// and stops it.
func (f *fleet) toldMetadata(generation uint64, env []string) string {
	f.t.Helper()
	told := "told-" + strconv.FormatUint(generation, 10)
	script := `printf '%s' "${` + MetadataHostVariable + `-unset}|${` + MetadataIPVariable + `-unset}" > /tmp/` + told + `.tmp && ` +
		`/bin/mv /tmp/` + told + `.tmp /tmp/` + told + `; exec /bin/sleep 600`
	if got := f.role("tester").start(shimwire.LauncherStart{
		ID: "start-" + strconv.FormatUint(generation, 10), Generation: generation, Argv: []string{"/bin/sh", "-c", script}, Env: env,
	}); !got.OK {
		f.t.Fatalf("start of generation %d = %#v; the launcher's log: %s", generation, got, f.stderr.String())
	}
	said := f.waitForRoleFile(f.roleTmp("tester", told))
	f.role("tester").stop(generation)
	return said
}

// The launcher listens before it serves any role and names the address it bound in its own
// environment, over whatever the container's environment carried, so its own Google clients (the
// session database's proxy) read it too; and the endpoint answers there for the pool it was named.
func TestMetadataLauncherServesAtItsAddressAndNamesItInItsOwnEnvironment(t *testing.T) {
	newMetadataFleet(t, "169.254.169.254", "127.0.0.1:0")
	address := servedMetadataAddress(t)
	if got := os.Getenv(MetadataIPVariable); got != address {
		t.Errorf("the launcher's own %s=%q once its roles connected, want %q as %s", MetadataIPVariable, got, address, MetadataHostVariable)
	}
	response, body := getMetadata(t, "http://"+address, metadataPrefix+"project/project-id", flavor)
	if response.StatusCode != http.StatusOK || body != "example-project" {
		t.Errorf("project-id at %s answered %d %q, want 200 example-project", address, response.StatusCode, body)
	}
}

// Every generation of a launcher that serves the endpoint is told the address it bound in both
// variables, over whatever the daemon's start command carried, so a role is told only the address
// its own launcher serves.
func TestEveryGenerationIsToldTheMetadataAddressItsLauncherServes(t *testing.T) {
	requireRoleNamespaces(t)
	f := newMetadataFleet(t, "", "127.0.0.1:0")
	address := servedMetadataAddress(t)
	want := address + "|" + address
	carried := []string{MetadataHostVariable + "=169.254.169.254", MetadataIPVariable + "=169.254.169.254"}
	if got := f.toldMetadata(1, carried); got != want {
		t.Errorf("a generation whose start command carried the node's address ran with %q, want %q", got, want)
	}
	if got := f.toldMetadata(2, nil); got != want {
		t.Errorf("a generation whose start command named no address ran with %q, want %q", got, want)
	}
}

// A launcher that serves no endpoint, a gVisor pod's, leaves each generation's environment as it
// is: a value its start command carried reaches it, and none is added.
func TestEveryGenerationIsToldTheMetadataAddressOfNoLauncherThatServesNone(t *testing.T) {
	holdMetadataVariables(t, "")
	none := newSessionRig(t, Config{})
	const expansion = "${" + MetadataHostVariable + "-unset}|${" + MetadataIPVariable + "-unset}"
	carried := []string{MetadataHostVariable + "=169.254.169.254", MetadataIPVariable + "=169.254.169.254"}
	if got := none.told(1, expansion, carried, ""); got != "169.254.169.254|169.254.169.254" {
		t.Errorf("a generation whose start command carried the node's address ran with %q, want it as carried", got)
	}
	if got := none.told(2, expansion, nil, ""); got != "unset|unset" {
		t.Errorf("a generation whose start command named no address ran with %q, want neither variable", got)
	}
}

// A launcher that cannot listen on the endpoint's address, which another process holds, says so
// in one line and names the address nowhere: not in its own environment, and (below) not to any
// generation.
func TestMetadataThatCannotListenIsOffAndNamedNowhere(t *testing.T) {
	f := newMetadataFleet(t, "", takeAPort(t))
	if !strings.Contains(f.stderr.String(), "launcher: metadata: off reason=") {
		t.Errorf("the launcher's log holds no off line: %s", f.stderr.String())
	}
	for _, name := range []string{MetadataHostVariable, MetadataIPVariable} {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("the launcher's own environment holds %s=%q, want none", name, value)
		}
	}
}

// Every generation of a launcher whose endpoint could not listen is told what its start command
// carried and nothing more, as a launcher that serves none tells it.
func TestEveryGenerationIsToldTheMetadataAddressOnlyByALauncherThatListens(t *testing.T) {
	requireRoleNamespaces(t)
	f := newMetadataFleet(t, "", takeAPort(t))
	carried := []string{MetadataHostVariable + "=169.254.169.254", MetadataIPVariable + "=169.254.169.254"}
	if got := f.toldMetadata(1, carried); got != "169.254.169.254|169.254.169.254" {
		t.Errorf("a generation whose start command carried the node's address ran with %q, want it as carried", got)
	}
	if got := f.toldMetadata(2, nil); got != "unset|unset" {
		t.Errorf("a generation whose start command named no address ran with %q, want neither variable", got)
	}
}

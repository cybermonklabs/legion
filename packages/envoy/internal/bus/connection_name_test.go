package bus

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// serverConnectionNames is every connection name the server's own census of its connections
// (/connz) lists.
func serverConnectionNames(t *testing.T, srv *testnats.Server) []string {
	t.Helper()
	response, err := http.Get(srv.MonitorURL() + "/connz?limit=1024")
	if err != nil {
		t.Fatalf("read /connz: %v", err)
	}
	defer response.Body.Close()
	var connz struct {
		Connections []struct {
			Name string `json:"name"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(response.Body).Decode(&connz); err != nil {
		t.Fatalf("decode /connz: %v", err)
	}
	names := make([]string, 0, len(connz.Connections))
	for _, c := range connz.Connections {
		names = append(names, c.Name)
	}
	return names
}

// TestTheConnectionNameTheServerSeesEndsWithTheBuild holds every connection this package opens to
// the binary's build after its name: a client's, under WithName and under the default name, and a
// Dial's. legacy-subjects-gate.sh's fact (a2) reads each connection's build from the end of the name
// the hub's census of connections lists.
func TestTheConnectionNameTheServerSeesEndsWithTheBuild(t *testing.T) {
	const build = "legion@0123456789abcdef0123456789abcdef01234567"
	previous := connectionBuild
	connectionBuild = func() string { return build }
	t.Cleanup(func() { connectionBuild = previous })

	srv, uri := testnats.Start(t)
	named, err := Connect([]string{uri}, WithName("envoy-listener-test"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(named.Close)
	unnamed, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(unnamed.Close)
	dialled, err := Dial("dial-test", []string{uri}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(dialled.Close)

	want := []string{"envoy-listener-test " + build, "envoy " + build, "dial-test " + build}
	deadline := time.Now().Add(10 * time.Second)
	for {
		names := serverConnectionNames(t, srv)
		missing := slices.DeleteFunc(slices.Clone(want), func(name string) bool { return slices.Contains(names, name) })
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server lists the connections %q, and none named %q", names, missing)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestARequiredModeListenersNameEndsWithTheSuffixAndNamesItsBuild: a client whose name ends with
// PrivacyRequiredSuffix, as a listener in required privacy mode names its connections, keeps the
// suffix last, since the auth service admits a listener on a hub with private projects only when
// its name ends with it, and still names its build, in the word before the suffix, where
// legacy-subjects-gate.sh reads it.
func TestARequiredModeListenersNameEndsWithTheSuffixAndNamesItsBuild(t *testing.T) {
	const build = "legion@0123456789abcdef0123456789abcdef01234567"
	previous := connectionBuild
	connectionBuild = func() string { return build }
	t.Cleanup(func() { connectionBuild = previous })

	srv, uri := testnats.Start(t)
	client, err := Connect([]string{uri}, WithName("envoy-listener-test"+PrivacyRequiredSuffix))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	want := "envoy-listener-test " + build + PrivacyRequiredSuffix
	deadline := time.Now().Add(10 * time.Second)
	for {
		names := serverConnectionNames(t, srv)
		if slices.Contains(names, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server lists the connections %q, and none named %q", names, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestAConnectionOfABuildThatHoldsNoCommitNamesNoBuild holds a binary with no build to name (a test
// binary, a build from a changed checkout) to its name alone, never a made-up build.
func TestAConnectionOfABuildThatHoldsNoCommitNamesNoBuild(t *testing.T) {
	previous := connectionBuild
	connectionBuild = func() string { return "" }
	t.Cleanup(func() { connectionBuild = previous })
	if got := connectionName("envoy"); got != "envoy" {
		t.Fatalf("connectionName(envoy) with no build = %q, want envoy", got)
	}
}

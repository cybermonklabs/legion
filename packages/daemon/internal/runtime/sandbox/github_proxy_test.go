package sandbox

import (
	"encoding/json"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// proxiedSecrets is the broker enrollment of the golden runtime with the tester's GitHub behind
// the broker's proxy, under one secret.
func proxiedSecrets() *AgentSecrets {
	return &AgentSecrets{
		URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour,
		GitHub:      map[claim.Role][]string{claim.RoleTester: {"LEGION_REVIEW_APP"}},
		GitHubHosts: []string{"api.github.com", "github.com", "uploads.github.com", "codeload.github.com"},
	}
}

// routeOver is the App-token bundle's rename rule (appauth.Route) over the configured smokeRename,
// with served the repositories the daemon serves: the rule a forwarder's bundle routes by.
func routeOver(served ...ghrepo.Repository) func(ghrepo.Repository) (string, []ghrepo.Rename) {
	renames := ghrepo.MustRenames(smokeRename)
	serves := servesOneOf(func() []ghrepo.Repository { return served })
	return func(repo ghrepo.Repository) (string, []ghrepo.Rename) { return appauth.Route(serves, renames, repo) }
}

// proxiedRuntime is the golden runtime with proxiedSecrets, routing as a daemon that serves
// servingSmoke, which smokeRename moved recordedSmoke (the test spec's repository) to.
func proxiedRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	r.agentSecrets = proxiedSecrets()
	r.gitHubRoute = routeOver(servingSmoke)
	return r
}

// forwarderFlags is the forwarder's flags a start command writes into the generation's
// directory, and whether the shim is told to run them.
func forwarderFlags(t *testing.T, r *Runtime, l launch) ([]string, bool) {
	t.Helper()
	start := launcherCommand(l, r)
	told := slices.Contains(start.Argv, "--github-forwarder")
	raw, written := start.Files[gitHubForwarderFile]
	if !written {
		if told {
			t.Fatalf("the shim is told --github-forwarder, but the start writes no %s", gitHubForwarderFile)
		}
		return nil, false
	}
	at := slices.Index(start.Argv, "--github-forwarder")
	if at < 0 || start.Argv[at+1] != generationDir(l.spec.Generation)+"/"+gitHubForwarderFile || at > slices.Index(start.Argv, "--") {
		t.Fatalf("the shim's argv %v does not name %s before --", start.Argv, gitHubForwarderFile)
	}
	var flags []string
	if err := json.Unmarshal([]byte(raw), &flags); err != nil {
		t.Fatalf("%s holds %q, no JSON list: %v", gitHubForwarderFile, raw, err)
	}
	return flags, true
}

// flagValues is every value of name in flags.
func flagValues(flags []string, name string) []string {
	var values []string
	for i := 0; i+1 < len(flags); i++ {
		if flags[i] == name {
			values = append(values, flags[i+1])
		}
	}
	return values
}

// A role the deployment's agent_secrets.github names reaches GitHub through its forwarder: its
// shim is told to run `legion forwarder` with the role's secret names, a reason naming the issue,
// a loopback port of the role's own, the gh bundle under the generation's private directory, the
// directory the issue's repository routes to as the default and the renames that resolve, both by
// the App-token bundle's rule (here smokeRename resolves, so the default is the owner it moved the
// repository to); its HTTPS_PROXY and https_proxy name that port; its gh variables name that
// bundle; and its gh volume projects nothing of its Secret. A role of the same pod the map does not
// name is launched exactly as before.
//
// The named revert: drop the proxy decision from the launch, and the tester's start names no
// forwarder.
func TestARoleBehindTheProxyIsStartedWithItsForwarder(t *testing.T) {
	r := proxiedRuntime(t)
	spec := workerSpec(t)
	l := launchOf(t, r, spec)
	flags, told := forwarderFlags(t, r, l)
	if !told {
		t.Fatal("the tester's start runs no forwarder")
	}
	dir := generationDir(spec.Generation) + "/gh"
	listen := "127.0.0.1:12804"
	for name, want := range map[string][]string{
		"--role": {"tester"}, "--listen": {listen}, "--secret": {"LEGION_REVIEW_APP"}, "--gh-dir": {dir},
		"--default-owner": {"cybermonklabs"}, "--rename": {"sjawhar/legion-smoke=cybermonklabs/legion-smoke"},
		"--github-host": {"api.github.com", "github.com", "uploads.github.com", "codeload.github.com"},
	} {
		if got := flagValues(flags, name); !slices.Equal(got, want) {
			t.Errorf("the forwarder's %s = %v, want %v", name, got, want)
		}
	}
	if reason := flagValues(flags, "--reason"); len(reason) != 1 || !strings.Contains(reason[0], spec.Issue) || !strings.Contains(reason[0], "tester") {
		t.Errorf("the forwarder's --reason = %v, want one naming the tester and issue %s", reason, spec.Issue)
	}
	env := envOf(workerOf(t, r, spec))
	for name, want := range map[string]string{
		"HTTPS_PROXY": "http://" + listen, "https_proxy": "http://" + listen,
		"LEGION_GH_OWNERS_DIR": dir, "GH_CONFIG_DIR": dir + "/cybermonklabs",
	} {
		if env[name] != want {
			t.Errorf("the tester is told %s=%q, want %q", name, env[name], want)
		}
	}
	pod := podOf(t, r, spec)
	if gh := volumeNamed(pod, roleVolume(ghVolume, claim.RoleTester)); gh == nil || gh.Secret != nil || gh.EmptyDir == nil {
		t.Errorf("the tester's gh volume is %+v, want an empty directory projecting nothing of its Secret", gh)
	}
	if gh := volumeNamed(pod, roleVolume(ghVolume, claim.RoleArchitect)); gh == nil || gh.Secret == nil {
		t.Errorf("the architect's gh volume is %+v, want its Secret's projection, as before", gh)
	}

	root := rootSpec(t)
	rootLaunch := launchOf(t, r, root)
	if _, told := forwarderFlags(t, r, rootLaunch); told {
		t.Error("the architect's start runs a forwarder the map does not give it")
	}
	rootEnv := envOf(workerOf(t, r, root))
	if _, set := rootEnv["HTTPS_PROXY"]; set || rootEnv["LEGION_GH_OWNERS_DIR"] != GHConfigDir {
		t.Errorf("the architect is told HTTPS_PROXY=%q LEGION_GH_OWNERS_DIR=%q, want no proxy and its projected bundle", rootEnv["HTTPS_PROXY"], rootEnv["LEGION_GH_OWNERS_DIR"])
	}
}

// volumeNamed is pod's volume of that name, or nil.
func volumeNamed(pod corev1.PodSpec, name string) *corev1.Volume {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == name {
			return &pod.Volumes[i]
		}
	}
	return nil
}

// A configured rename that does not resolve — the daemon does not serve the repository it moves a
// repository to — routes nothing for a role behind the proxy, as it routes nothing in the App-token
// bundle (appauth.Route, ghrepo.Renames.Moving): no --rename, and the default is the issue's own
// owner.
//
// The named revert: pass the forwarder every configured pair that crosses owners, and the tester
// is told the pair and routed to its `to` owner.
func TestAForwarderRoutesByTheAppTokenBundlesRule(t *testing.T) {
	r := proxiedRuntime(t)
	r.gitHubRoute = routeOver(recordedSmoke)
	flags, told := forwarderFlags(t, r, launchOf(t, r, workerSpec(t)))
	if !told {
		t.Fatal("the tester's start runs no forwarder")
	}
	if got := flagValues(flags, "--rename"); len(got) != 0 {
		t.Errorf("the forwarder is told renames %v; want none, as the pair does not resolve", got)
	}
	if got := flagValues(flags, "--default-owner"); !slices.Equal(got, []string{"sjawhar"}) {
		t.Errorf("the forwarder's --default-owner = %v, want [sjawhar], the issue's own owner", got)
	}
}

// No role the deployment's agent_secrets.github leaves out is started with a forwarder: its shim
// is told no --github-forwarder, which a worker image from before the flag would refuse, and its
// start writes no forwarder file and its environment names no proxy. Here the map names the tester
// alone, and every other role, the controller included, is checked.
func TestARoleTheMapLeavesOutIsToldNoForwarderFlag(t *testing.T) {
	r := proxiedRuntime(t)
	for _, role := range forwarderRoles {
		if role == claim.RoleTester {
			continue
		}
		spec := roleSpec(t, role)
		start := launcherCommand(launchOf(t, r, spec), r)
		if slices.Contains(start.Argv, "--github-forwarder") {
			t.Errorf("the %s's shim is told --github-forwarder: %v", role, start.Argv)
		}
		if _, written := start.Files[gitHubForwarderFile]; written {
			t.Errorf("the %s's start writes %s", role, gitHubForwarderFile)
		}
		if proxy, set := envOf(workerOf(t, r, spec))["HTTPS_PROXY"]; set {
			t.Errorf("the %s is told HTTPS_PROXY=%q", role, proxy)
		}
	}
}

// Each role's forwarder listens on a loopback port of its own, since every role of an issue pod
// shares the pod's network namespace.
func TestEveryRolesForwarderHasAPortOfItsOwn(t *testing.T) {
	want := map[claim.Role]string{
		claim.RoleArchitect: "127.0.0.1:12801", claim.RolePlanner: "127.0.0.1:12802", claim.RoleImplementer: "127.0.0.1:12803",
		claim.RoleTester: "127.0.0.1:12804", claim.RoleReviewer: "127.0.0.1:12805", claim.RoleMerger: "127.0.0.1:12806",
		claim.RoleController: "127.0.0.1:12807",
	}
	for role, address := range want {
		if got := forwarderAddress(role); got != address {
			t.Errorf("forwarderAddress(%s) = %s, want %s", role, got, address)
		}
	}
}

// No forwarder's port is one the kernel may hand an outgoing connection as its source port
// before the forwarder binds it: each is below the first ephemeral port of a gVisor pod (16000,
// gVisor's netstack firstEphemeral, pkg/tcpip/ports/ports.go), below Linux's default range
// (32768-60999, the range a runc pod and a developer box read in ip_local_port_range), and below
// the 2000 ports the load test takes just under a gVisor pod's range (14000-15999,
// packages/envoy/loadtest/common.sh).
//
// The named revert: put the ports back at 47801 and up, and every one is inside both ranges.
func TestNoForwarderPortIsAnEphemeralOne(t *testing.T) {
	const belowEverything = 14000
	for _, role := range forwarderRoles {
		_, portText, err := net.SplitHostPort(forwarderAddress(role))
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal(err)
		}
		if port >= belowEverything || port < 1024 {
			t.Errorf("the %s's forwarder port %d is not in 1024-%d, below every ephemeral range a pod may have", role, port, belowEverything-1)
		}
	}
}

// With no agent_secrets.github, no role is started with a forwarder or a proxy, and every gh
// volume projects the role's Secret as before.
func TestNoMapStartsNoForwarder(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	r.agentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	spec := workerSpec(t)
	if _, told := forwarderFlags(t, r, launchOf(t, r, spec)); told {
		t.Fatal("a runtime with no github map starts a forwarder")
	}
	if _, set := envOf(workerOf(t, r, spec))["HTTPS_PROXY"]; set {
		t.Fatal("a runtime with no github map tells a role HTTPS_PROXY")
	}
	if gh := volumeNamed(podOf(t, r, spec), roleVolume(ghVolume, claim.RoleTester)); gh == nil || gh.Secret == nil {
		t.Fatalf("the tester's gh volume is %+v, want its Secret's projection", gh)
	}
}

// A launch spec may not set HTTPS_PROXY or https_proxy where the deployment puts a role's GitHub
// behind the proxy: the runtime sets both for such a role. They are reserved beside whatever the
// deployment's exchange reserves (PodExchange), dispatch_token_broker's DISPATCH_TOKEN_BROKER
// included, and not at all where no github map is set.
func TestTheProxyVariablesAreTheRuntimesWhereTheMapIsSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		audience string
		broker   bool
	}{
		{name: "no exchange"},
		{name: "an exchange identity", audience: testExchangeAudience},
		{name: "an exchange identity under dispatch_token_broker", audience: testExchangeAudience, broker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proxiedRuntime(t)
			r.agentSecrets.ExchangeAudience, r.dispatch.Broker = tc.audience, tc.broker
			proxied := r.specOwned()
			r.agentSecrets.GitHub = nil
			unproxied := r.specOwned()
			if unproxied["HTTPS_PROXY"] || unproxied["https_proxy"] {
				t.Error("specOwned takes a proxy variable where no github map is set")
			}
			want := maps.Clone(unproxied)
			want["HTTPS_PROXY"], want["https_proxy"] = true, true
			if !maps.Equal(proxied, want) {
				t.Errorf("specOwned with the github map set is %v, want what it takes without the map and the two proxy variables: %v",
					slices.Sorted(maps.Keys(proxied)), slices.Sorted(maps.Keys(want)))
			}
		})
	}
}

// The tester's Secret holds no GitHub token where its GitHub is behind the proxy: no hosts.yml key
// is written, its recorded default is empty so the refresher writes none, and a refresh tick adds
// none. The architect beside it still gets its App's bundle.
func TestARoleBehindTheProxyGetsNoAppToken(t *testing.T) {
	g := newRig(t, nil, func(_ *rig, o *Options) { o.AgentSecrets, o.GitHubRoute = proxiedSecrets(), routeOver(servingSmoke) })
	loc := g.spawn(workerSpec(t))
	g.r.refreshGitHubCredentials(g.ctx)
	secret := g.secret(roleSecretName(loc.Sandbox.Name, claim.RoleTester))
	if secret == nil {
		t.Fatal("the tester has no Secret")
	}
	for key, value := range secret.Data {
		if strings.Contains(string(value), "ghs_") {
			t.Errorf("the tester's Secret holds a GitHub token under %s, want none", key)
		}
	}
	if got, ok := secret.Data[GitHubDefaultKey]; !ok || len(got) != 0 {
		t.Errorf("the tester's Secret records default %q (%v), want an empty one", got, ok)
	}
	architect := g.secret(roleSecretName(loc.Sandbox.Name, claim.RoleArchitect))
	if architect == nil || string(architect.Data[GitHubHostsKey("sjawhar")]) == "" {
		t.Errorf("the architect's Secret holds no sjawhar hosts.yml, want its App's bundle as before")
	}
	start := lastStart(t, g, workerToken)
	if !slices.Contains(start.Argv, "--github-forwarder") {
		t.Errorf("the tester's start %v names no forwarder", start.Argv)
	}
}

// A pod whose tester projects its App's tokens has drifted once the deployment puts the tester's
// GitHub behind the proxy, so the update round replaces it; a pod built with the tester behind the
// proxy has not, and a launch that keeps such a pod starts the tester with its forwarder, where a
// launch that keeps the older pod starts it as that pod was built.
func TestAPodWhoseRoleProjectsTokensDriftsOnceTheRoleIsBehindTheProxy(t *testing.T) {
	before, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	before.agentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
	after := proxiedRuntime(t)
	spec := workerSpec(t)
	for name, tc := range map[string]struct {
		built   *Runtime
		drifted bool
	}{
		"a pod built before the map": {before, true},
		"a pod built under the map":  {after, false},
	} {
		t.Run(name, func(t *testing.T) {
			podSpec := podOf(t, tc.built, spec)
			pod := &corev1.Pod{Spec: podSpec}
			moved, drifted := after.movedGhDirs(pod, claim.RoleTester)
			if drifted != tc.drifted {
				t.Fatalf("movedGhDirs = %v, %v; want drifted %v", moved, drifted, tc.drifted)
			}
			if after.proxiesGitHubIn(&podSpec, claim.RoleTester) == tc.drifted {
				t.Fatalf("proxiesGitHubIn of %s = %v, want %v", name, !tc.drifted, !tc.drifted)
			}
		})
	}
}

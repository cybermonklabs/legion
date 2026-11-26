package sandbox

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A role whose GitHub the deployment puts behind the secrets broker's proxy
// (runtime.kubernetes.agent_secrets.github) reaches GitHub through its own forwarder, `legion
// forwarder` (internal/githubproxy), which its shim starts with the generation: the forwarder asks
// the broker, with the role's own key, for an hour's grant of the role's secret names, writes each
// account's placeholder into a gh bundle under the generation's private directory, and carries
// the role's CONNECTs to GitHub's hosts to the broker's proxy port. The daemon mints no App token
// for such a role and writes none into its pod; the role's environment names the forwarder in
// HTTPS_PROXY and https_proxy, and the bundle in LEGION_GH_OWNERS_DIR and GH_CONFIG_DIR.

// gitHubForwarderFile is the file of a generation's private directory holding the forwarder's
// flags as a JSON list, which the start command writes (launcherCommand) and the shim's
// --github-forwarder names.
const gitHubForwarderFile = "github-forwarder.json"

// forwarderPortBase is the loopback port before the first role's forwarder's: every role of an
// issue pod shares the pod's network namespace, so each role's forwarder listens on a port of its
// own, forwarderPortBase plus the role's place in forwarderRoles (12801-12807). The ports are
// below every range the kernel takes a connection's source port from, so no outgoing connection
// of the pod can hold one before the forwarder binds it: a gVisor pod's netstack starts its range
// at 16000 (gVisor's pkg/tcpip/ports firstEphemeral), Linux's default range is 32768-60999, and
// the load test takes 14000-15999 just under a gVisor pod's range
// (packages/envoy/loadtest/common.sh).
const forwarderPortBase = 12800

// forwarderRoles are the roles a forwarder may serve, each the place its port is counted from:
// every launcher role, the workflow roles and then the controller (launcherRoles).
var forwarderRoles = slices.Clone(launcherRoles)

// forwarderAddress is the loopback address role's forwarder listens on, which its HTTPS_PROXY
// names.
func forwarderAddress(role claim.Role) string {
	return "127.0.0.1:" + strconv.Itoa(forwarderPortBase+1+slices.Index(forwarderRoles, role))
}

// proxyOwned are the proxy variables the runtime sets for a role behind the broker's proxy
// (ghAgentEnv), which a launch spec may not set where the deployment puts any role there
// (specOwned).
var proxyOwned = map[string]bool{"HTTPS_PROXY": true, "https_proxy": true}

// withProxyOwned is owned and proxyOwned together, built once per owned set.
func withProxyOwned(owned map[string]bool) map[string]bool {
	names := maps.Clone(owned)
	maps.Copy(names, proxyOwned)
	return names
}

// runtimeOwnedComposedOrProxy, runtimeOwnedComposedExchangeOrProxy and
// runtimeOwnedComposedExchangeBrokerOrProxy are runtimeOwnedOrComposed,
// runtimeOwnedComposedOrExchange and runtimeOwnedComposedExchangeOrBroker with proxyOwned, what a
// launch spec may not set where the deployment puts any role's GitHub behind the proxy
// (specOwned).
var (
	runtimeOwnedComposedOrProxy               = withProxyOwned(runtimeOwnedOrComposed)
	runtimeOwnedComposedExchangeOrProxy       = withProxyOwned(runtimeOwnedComposedOrExchange)
	runtimeOwnedComposedExchangeBrokerOrProxy = withProxyOwned(runtimeOwnedComposedExchangeOrBroker)
)

// proxiesGitHubIn is the one decision whether a generation of role started now in a pod reaches
// GitHub through its forwarder: the generation holds the broker (enrollsIn), whose key the
// forwarder asks with; the deployment names secrets for the role (AgentSecrets.GitHub); and the
// pod's gh volume for the role projects nothing of its Secret (ghProjectionOf), which is how a pod
// built with the role behind the proxy is told from one built with its App's tokens. A nil pod is
// a launch that builds the pod itself, which carries the configuration as it now reads. A role in
// a kept pod built with its App's tokens is started as that pod was built, its tokens refreshed,
// until the update round replaces the pod (movedGhDirs).
func (r *Runtime) proxiesGitHubIn(pod *corev1.PodSpec, role claim.Role) bool {
	if !r.enrollsIn(pod, role) || len(r.agentSecrets.GitHub[role]) == 0 {
		return false
	}
	if pod == nil {
		return true
	}
	projected, found := ghProjectionOf(&corev1.Pod{Spec: *pod}, role)
	return found && !projected.secret
}

// proxiesGitHub is this launch's reading of that decision: of the live pod it keeps, as the launch
// read that pod's stored template when it began (keptPod.proxied), or of the pod it builds itself.
func (l launch) proxiesGitHub() bool {
	if l.kept != nil {
		return l.kept.proxied
	}
	return l.proxied
}

// gitHubProxyDir is the gh bundle a generation's forwarder writes, under the generation's own
// private directory, which the launcher makes before the shim starts and removes when the
// generation ends: no placeholder of a grant outlives the generation that asked for it.
func gitHubProxyDir(generation uint64) string {
	return generationDir(generation) + "/gh"
}

// proxyRoute is where a role behind the proxy is routed, decided once in prepare: the account
// directory GH_CONFIG_DIR names and the configured renames its forwarder's bundle routes by, both
// by the rule the App-token bundle routes by (Options.GitHubRoute, appauth.Route), so a role
// routes one repository alike whichever bundle it reads. The directory is the one the issue's
// repository routes to and, for the controller, which works no repository, the first owner the
// daemon serves (Options.GitHubDirs), "" where it serves none. It mints nothing.
func (r *Runtime) proxyRoute(spec runtime.SpawnSpec) (string, []ghrepo.Rename) {
	dir, pairs := r.gitHubRoute(spec.Repository)
	if spec.Role == claim.RoleController {
		dir = ""
		if owners := ghOwnerDirs(r.gitHubDirs(ghrepo.Repository{}, podAppRoles(controllerPod{}))); len(owners) > 0 {
			dir = slices.Min(owners)
		}
	}
	return dir, pairs
}

// forwarderFlags are the flags l's forwarder is started with (`legion forwarder`): its role, its
// loopback address, the role's secret names, a reason naming the role and its issue (the grant's
// audit row names it), the hosts the broker's GitHub kind binds, the bundle it writes, the
// directory GH_CONFIG_DIR names and the renames (proxyRoute).
func (r *Runtime) forwarderFlags(l launch) []string {
	role := l.spec.Role
	reason := fmt.Sprintf("GitHub access of the %s of %s, through its forwarder", role, l.spec.Issue)
	if role == claim.RoleController {
		reason = fmt.Sprintf("GitHub access of project %s's controller, through its forwarder", l.spec.Project)
	}
	flags := []string{"--role", string(role), "--listen", forwarderAddress(role)}
	for _, name := range r.agentSecrets.GitHub[role] {
		flags = append(flags, "--secret", name)
	}
	flags = append(flags, "--reason", reason)
	for _, host := range r.agentSecrets.GitHubHosts {
		flags = append(flags, "--github-host", host)
	}
	flags = append(flags, "--gh-dir", gitHubProxyDir(l.spec.Generation))
	if l.proxyDefault != "" {
		flags = append(flags, "--default-owner", l.proxyDefault)
	}
	for _, pair := range l.proxyPairs {
		flags = append(flags, "--rename", pair.From.String()+"="+pair.To.String())
	}
	return flags
}

// forwarderFile is forwarderFlags as the file the shim reads them from.
func (r *Runtime) forwarderFile(l launch) string {
	encoded, err := json.Marshal(r.forwarderFlags(l))
	if err != nil {
		panic(err) // a list of strings always encodes
	}
	return string(encoded)
}

// proxiesAnyGitHub is whether the deployment puts any role's GitHub behind the broker's proxy.
func (r *Runtime) proxiesAnyGitHub() bool {
	return r.agentSecrets != nil && len(r.agentSecrets.GitHub) > 0
}

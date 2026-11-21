package gcp

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/infra/core"
)

// privateModeListenerFloor is the first listener release, as major.minor.patch, that enforces the
// required mode: whose listener reads ENVOY_PROJECT_PRIVACY_REQUIRED and the privacy route, and so
// holds each broker-token caller to the projects it may see (plan C's t-c-8). A private hub's
// session listener renders the broker check, which turns broker tokens on in the listener; a
// listener of an earlier release admits every live broker token under DPoP and limits none of
// them by project, so with privateProjects the listener's image must name a release at or after
// this one (checkPrivateModeListenerImage). It is empty until t-c-8's release ships, which sets
// it, and empty refuses every image: no private hub runs a session listener before then. A
// package-level variable, so the tests set it to render the private listener (export_test.go).
var privateModeListenerFloor = ""

// imageReleaseTag is the release a listener image's tag names, <name>:<tag>@sha256:<digest>, the
// tag ending in v<major>.<minor>.<patch> (a release's version, or its name, legion-envoy-v…).
var imageReleaseTag = regexp.MustCompile(`:(?:[A-Za-z0-9._-]*-)?v([0-9]+)\.([0-9]+)\.([0-9]+)@sha256:[0-9a-f]{64}$`)

// checkPrivateModeListenerImage refuses image, the session listener's of a hub with
// privateProjects, unless it names a release at or after privateModeListenerFloor. With no floor
// named it refuses every image, naming privateProjects; an image whose tag names no release, or an
// earlier one, is refused naming <path>.image, since its digest alone says nothing the component
// could compare.
func checkPrivateModeListenerImage(path, image string) error {
	if privateModeListenerFloor == "" {
		return core.InputErrorf("privateProjects", "privateProjects needs a session listener release that enforces the required mode; none is named yet (privateModeListenerFloor): a %s of an earlier release admits every live broker token under DPoP and holds none of its callers to the projects they may see", path)
	}
	floor, ok := releaseVersion(":v" + privateModeListenerFloor + "@sha256:" + strings.Repeat("0", 64))
	if !ok {
		return errors.New("privateModeListenerFloor " + strconv.Quote(privateModeListenerFloor) + " is no major.minor.patch")
	}
	release, ok := releaseVersion(image)
	if !ok || slices.Compare(release, floor) < 0 {
		return core.InputErrorf(path+".image", "%s.image %q must name, as its tag, a listener release at or after v%s, the first that enforces the required mode, as <name>:v%s@sha256:<digest>: privateProjects turns broker tokens on in the listener, and an earlier one holds none of their callers to the projects they may see",
			path, image, privateModeListenerFloor, privateModeListenerFloor)
	}
	return nil
}

// releaseVersion is the major, minor and patch numbers image's tag names, and whether it names any.
func releaseVersion(image string) ([]int, bool) {
	parts := imageReleaseTag.FindStringSubmatch(image)
	if parts == nil {
		return nil, false
	}
	version := make([]int, 3)
	for i, part := range parts[1:] {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		version[i] = n
	}
	return version, true
}

// clusteredHubListener is the resolved session listener the hub runs as a workload of its own,
// nil without one: the tailnet hosts forward the Tailscale Service's port 9020 to it, and the
// pods of clientNamespaces reach it at its cluster Service. env is its environment but the seed
// and its credential (core.ListenerConfig.Env), and credential is the token check it requires of
// every caller, nil where it serves with none (clusteredHubListenerEnv). natsAudience is set
// behind the auth service, where the listener presents a projected token for it and the seed's
// Secret is empty; it is empty otherwise, where the pod mounts that Secret. privacy is what the
// listener of a hub that may make a project private reads privacy with, nil on any other hub.
type clusteredHubListener struct {
	image, machineID              string
	seedSecretName, seedSecretKey string
	natsAudience                  string
	clientNamespaces              []string
	cpu, memory                   string
	env                           []string
	credential                    *clusteredHubListenerCredential
	privacy                       *clusteredHubListenerPrivacy
}

// clusteredHubListenerPrivacy is what a private hub's listener reads privacy and proofs with that
// its inputs do not say as they are: the audience of the projected token of its own account it
// presents to the secrets broker, mounted at clusteredHubListenerBrokerTokenFile; the one origin
// it checks every caller's DPoP proof against (ENVOY_PUBLIC_URL); and the namespaces of its
// all-projects callers, which the listener's NetworkPolicy admits at its port and which reach its
// replicas at the headless Service clusteredHubListenerInstancesName names.
type clusteredHubListenerPrivacy struct {
	brokerAudience string
	origin         string
	namespaces     []string
}

// clusteredHubListenerCredential is the token check a listener has: the issuer and audience of
// the projected ServiceAccount tokens it verifies, and the subjects it admits. open is a private
// hub's tailnet-only listener's, which checks a token that is presented and keeps /v1 open to a
// caller that presents none (core.ListenerConfig.V1OpenWithTokenChecks); every other listener with
// a token check requires one of every caller.
type clusteredHubListenerCredential struct {
	issuer, audience string
	accounts         []string
	open             bool
}

// The per-source-address introspection limit a private hub's session listener renders with tailnet
// (core.ListenerConfig.IntrospectionsPerSecond, IntrospectionBurst). Every tailnet session reaches
// the listener through the tailnet hosts, so every one of them shares a host's address, and the
// listener's default bucket (five a second, a burst of 20, keyed on the source address) is then one
// bucket per host for the whole tailnet: a session spends one introspection per 5 s answer cache
// while it calls and at least one per 90 s registration heartbeat, so two hosts at the default
// saturate near 2 × 5 × 90 = 900 sessions, and a restart that registers every session again passes
// 40 at once. The limit here is the secrets broker's own bound on the listener: the broker spends
// one request of its per-source-address bucket on each POST /v1/oauth/introspect
// (internal/cipher/api's DefaultAccountLimit, fifty a second and a burst of 200, per broker pod),
// and the listener pod is that one source address. A host's bucket the size of the broker's own
// never refuses a session the broker would have admitted, and the broker's bucket stays the bound
// on a flood. So one host carries 50 × 90 = 4,500 idle sessions, as many after the other host is
// drained, and a restart storm passes 200 at once per host and then fifty a second.
// cmd/listener's TestTheRenderedIntrospectionLimitIsTheBrokers holds the two to each other.
const (
	clusteredHubListenerIntrospectionsPerSecond = 50
	clusteredHubListenerIntrospectionBurst      = 200
)

// clusteredHubListenerArgs is the session listener the hub runs, wherever it is declared, and the
// path its refusals name: listener, the hub's own input, or tailnet.listener, the same listener
// declared inside the tailnet entry, which renders the same objects but for the listener
// container's token check (clusteredHubListenerCredentialFor). Nil and "" with neither. The hub
// runs one listener, so the two are refused together.
func clusteredHubListenerArgs(args *ClusteredNatsHubArgs) (*ClusteredHubListenerArgs, string, error) {
	var inTailnet *ClusteredHubListenerArgs
	if args.Tailnet != nil {
		inTailnet = args.Tailnet.Listener
	}
	switch {
	case args.Listener != nil && inTailnet != nil:
		return nil, "", core.InputErrorf("listener", "listener and tailnet.listener: the hub runs one session listener, and tailnet.listener is that listener declared inside the tailnet entry; declare it once")
	case args.Listener != nil:
		return args.Listener, "listener", nil
	case inTailnet != nil:
		return inTailnet, "tailnet.listener", nil
	}
	return nil, "", nil
}

// newClusteredHubListenerSettings resolves the session listener l, declared at path, and the
// listener user the hub renders for it: both nil without one, where natsReplicas, which only the
// listener takes, is refused, and so is tailnet, whose hosts forward the Tailscale Service's port
// 9020 to it. The listener runs as its machine id in a pod of its own and serves on every address
// of that pod on the listener's default port, reached through its cluster Service, which a
// NetworkPolicy admits the tailnet entry's pods and the pods of clientNamespaces alone to, and
// where each tailnet host forwards the Tailscale Service's port 9020; it reaches NATS through the
// hub's client Service. Its machine id is tailnet.serviceName by default with tailnet and
// required without it, where no other name of the hub's is the listener's. Without tailnet,
// clientNamespaces names at least one namespace, since nothing else reaches the listener. The
// credential it requires of a caller is clusteredHubListenerCredentialFor's to resolve. A listener
// that requires none answers on the Service's tailnet name and on every further address
// allowedHosts names; the operator's entries are checked as their own list, so a refusal names
// <path>.allowedHosts and the index the operator wrote. Its sender limit is core's rule
// (core.CheckSenderLimit), naming <path>.senderMessagesPerMinute or <path>.senderMessageBurst.
//
// Its NATS identity follows authCallout (listenerIdentity): without it, the user the hub renders
// for the listener's seed, from the Secret the pod mounts; with it, the projected token of the
// listener's own ServiceAccount for natsAudience, which the pod reads at
// clusteredHubListenerTokenFile, while the hub renders no user for it. With privateProjects it also
// renders the broker check and the privacy settings (newClusteredHubListenerPrivacy).
func newClusteredHubListenerSettings(args *ClusteredNatsHubArgs, l *ClusteredHubListenerArgs, path string, s clusteredHubSettings) (*clusteredHubListener, *listenerUser, error) {
	if l == nil {
		if args.Tailnet != nil {
			return nil, nil, core.InputErrorf("tailnet.listener", "tailnet.listener is required with tailnet: the tailnet hosts forward the Tailscale Service's port %d to the session listener the hub runs as a workload of its own; declare it as tailnet.listener or as listener", core.DefaultListenerPort)
		}
		if args.NatsReplicas != nil {
			return nil, nil, core.InputErrorf("natsReplicas", "natsReplicas is given without a session listener (listener or tailnet.listener), which alone takes it (ENVOY_NATS_REPLICAS)")
		}
		return nil, nil, nil
	}
	if !imageRule.MatchString(l.Image) {
		return nil, nil, core.InputErrorf(path+".image", "%s.image %q must be pinned by digest, as <name>@sha256:<64 lowercase hex digits>", path, l.Image)
	}
	var machineID string
	switch {
	case l.MachineID != nil:
		machineID = *l.MachineID
	case s.tailnet != nil:
		machineID = s.tailnet.serviceName
	default:
		return nil, nil, core.InputErrorf(path+".machineId", "%s.machineId is required without tailnet, whose serviceName is the listener's machine id by default", path)
	}
	if err := core.CheckMachineID(path+".machineId", machineID); err != nil {
		return nil, nil, err
	}
	service := args.AuthCallout != nil
	if err := listenerIdentity(path+".", service, l.NatsAudience, "a projected ServiceAccount token's audience", "BROKER_NATS_K8S_AUDIENCE",
		[][2]string{{"nkey", l.NKey}, {"seedSecretName", l.SeedSecretName}, {"seedSecretKey", l.SeedSecretKey}}); err != nil {
		return nil, nil, err
	}
	if !service {
		if l.NKey == "" {
			return nil, nil, core.InputErrorf(path+".nkey", "%s.nkey is required", path)
		}
		if !dnsSubdomain(l.SeedSecretName) {
			return nil, nil, core.InputErrorf(path+".seedSecretName", "%s.seedSecretName %q must be a DNS subdomain, the name of a Secret in namespace %s", path, l.SeedSecretName, s.namespace)
		}
		if !secretKeyRule.MatchString(l.SeedSecretKey) {
			return nil, nil, core.InputErrorf(path+".seedSecretKey", "%s.seedSecretKey %q must match %s", path, l.SeedSecretKey, secretKeyRule)
		}
	}
	if err := checkNamespaces(path+".clientNamespaces", l.ClientNamespaces); err != nil {
		return nil, nil, err
	}
	if s.tailnet == nil && len(l.ClientNamespaces) == 0 {
		return nil, nil, core.InputErrorf(path+".clientNamespaces", "%s.clientNamespaces: at least one namespace is required without tailnet: no tailnet host forwards to the listener, and its NetworkPolicy admits nothing else, so nothing could reach it", path)
	}
	if err := core.CheckAllowedHosts(path+".allowedHosts", l.AllowedHosts); err != nil {
		return nil, nil, err
	}
	rate, burst, err := core.CheckSenderLimit(path+".", l.SenderMessagesPerMinute, l.SenderMessageBurst)
	if err != nil {
		return nil, nil, err
	}
	credential, err := clusteredHubListenerCredentialFor(args, l, path)
	if err != nil {
		return nil, nil, err
	}
	privacy, err := newClusteredHubListenerPrivacy(args, l, path, credential, s.tailnet)
	if err != nil {
		return nil, nil, err
	}
	listener := &clusteredHubListener{
		image:            l.Image,
		machineID:        machineID,
		seedSecretName:   l.SeedSecretName,
		seedSecretKey:    l.SeedSecretKey,
		natsAudience:     l.NatsAudience,
		clientNamespaces: l.ClientNamespaces,
		credential:       credential,
		privacy:          privacy,
	}
	// The listener is the only container of its own pod, so its size is that pod's whole request,
	// refused where Autopilot would raise or reject the pod, as the hub pod's size is.
	if listener.cpu, listener.memory, err = autopilotPodSize(autopilotPod{
		name:     "the listener pod",
		cpuInput: path + ".cpu", memoryInput: path + ".memory",
		cpu: l.Cpu, memory: l.Memory,
		defaultCPU: defaultClusteredHubListenerCPU, defaultMemory: defaultClusteredHubListenerMemory,
		memoryExamples: "1Gi or 512Mi",
	}); err != nil {
		return nil, nil, err
	}
	// The names a client reaches /v1 by with no credential, in a fixed order: the tailnet name the
	// Tailscale Service answers at, then the operator's own. Such a listener is a tailnet.listener
	// with no clientNamespaces, so the list always holds the tailnet name: one with no token check,
	// or a private hub's, which keeps its open /v1 beside the check. One that requires a credential
	// of every caller serves /v1 to any Host and reads no ENVOY_ALLOWED_HOSTS
	// (cmd/listener/apiauth.go), so it renders none.
	var hosts []string
	if credential == nil || credential.open {
		hosts = append([]string{core.TailnetHostName(s.tailnet.serviceName, s.tailnet.tailnetDNSName)}, l.AllowedHosts...)
	}
	// The listener serves on every address of its own pod: the tailnet hosts and the pods of
	// clientNamespaces reach it through its cluster Service, which a NetworkPolicy admits their
	// pods alone to. A listener with a credential admits only the callers that present it; the
	// tailnet-only one admits every caller the policy lets through, with no credential.
	config := core.ListenerConfig{
		MachineID:               machineID,
		Port:                    core.DefaultListenerPort,
		ListenHost:              "0.0.0.0",
		AllowedHosts:            hosts,
		NATSReplicas:            s.natsReplicas,
		StreamMaxBytes:          s.streamMaxBytes,
		SenderMessagesPerMinute: rate,
		SenderMessageBurst:      burst,
	}
	if service {
		config.ServiceTokenFile = clusteredHubListenerTokenFile
	}
	// A private hub's listener reads each decision's privacy from the secrets broker in its
	// required mode, with the projected token of its own account, and so takes broker tokens too:
	// the broker check in its token-file form, and the privacy settings, from its own inputs and,
	// for the flag, from the hub's privateProjects alone. Any other hub's listener renders none of
	// them, whatever the inputs hold (R9).
	if privacy != nil {
		config.AgentSecretsURL, config.BrokerTokenFile, config.PublicURL = l.AgentSecretsUrl, clusteredHubListenerBrokerTokenFile, privacy.origin
		config.PrivacySourceURL, config.PrivacyRequired, config.AllProjectsAccounts = l.PrivacySourceUrl, args.PrivateProjects, l.AllProjectsAccounts
		config.V1OpenWithTokenChecks = credential.open
		if s.tailnet != nil {
			config.IntrospectionsPerSecond, config.IntrospectionBurst = clusteredHubListenerIntrospectionsPerSecond, clusteredHubListenerIntrospectionBurst
		}
	}
	// What is about to be rendered is held to the hub's privacy mode before anything registers, so a
	// render that drops a piece is refused here, naming the listener and the piece.
	if err := core.CheckPrivacySource(path, config, args.PrivateProjects); err != nil {
		return nil, nil, err
	}
	if listener.env, err = config.Env([]string{s.serviceURL}); err != nil {
		return nil, nil, prefixedInput(path, err)
	}
	return listener, &listenerUser{machineID: machineID, nkey: l.NKey}, nil
}

// prefixedInput is err, core's refusal of a listener config, with the input it names prefixed by
// path, the hub's own path for its listener, where that input is one the listener's args carry
// under the same name: the broker check's and the privacy settings'. core names its own fields,
// which a consumer writes as <path>.<input>; any other error passes unchanged.
func prefixedInput(path string, err error) error {
	var input *core.InputError
	if !errors.As(err, &input) {
		return err
	}
	switch input.Input {
	case "agentSecretsUrl", "publicUrl", "privacySourceUrl", "allProjectsAccounts":
		return core.InputErrorf(path+"."+input.Input, "%s.%w", path, input.Err)
	}
	return err
}

// newClusteredHubListenerPrivacy resolves what the listener l, declared at path, of a hub with
// privateProjects reads privacy with: nil on a hub without it, where the privacy inputs are read
// nowhere. Such a listener has a token check (credential, which clusteredHubListenerCredentialFor
// requires of it), since its readiness route knows Dispatch's account by its projected token. A
// tailnet-only one keeps its open /v1 beside the check (credential.open), so the private mode
// narrows no caller before the open /v1 is removed. agentSecretsUrl, brokerAudience,
// privacySourceUrl and allProjectsAccounts are each required, by its path. brokerAudience is
// refused when it is natsAudience's: that token is the auth service's, which the broker's routes
// do not accept. Each all-projects caller is a subject the token check admits, since the listener
// verifies a caller before it serves it every project's sessions, and its namespace is admitted at
// the listener's port. The values' shapes are core's rules (core.ListenerConfig.Check,
// core.CheckBroker), named by path.
//
// The listener checks every DPoP proof against one origin, ENVOY_PUBLIC_URL, and the callers that
// present a broker token to it are the tailnet's sessions, which reach it at the Tailscale
// Service's name. So with tailnet its origin is that name's, http://<serviceName>.<tailnetDnsName>
// :9020 (the hub's tailnetEnvoyUrl), whatever publicUrl says: an unset publicUrl takes it, and any
// other value is refused, naming publicUrl, since a proof a caller signs for the tailnet name would
// then be refused. Without tailnet, publicUrl is required, the one origin its callers reach it at.
func newClusteredHubListenerPrivacy(args *ClusteredNatsHubArgs, l *ClusteredHubListenerArgs, path string, credential *clusteredHubListenerCredential, tailnet *clusteredHubTailnet) (*clusteredHubListenerPrivacy, error) {
	if !args.PrivateProjects {
		return nil, nil
	}
	if err := checkPrivateModeListenerImage(path, l.Image); err != nil {
		return nil, err
	}
	for _, in := range []struct {
		name string
		set  bool
	}{
		{"agentSecretsUrl", l.AgentSecretsUrl != ""},
		{"publicUrl", l.PublicUrl != "" || tailnet != nil},
		{"brokerAudience", l.BrokerAudience != ""},
		{"privacySourceUrl", l.PrivacySourceUrl != ""},
		{"allProjectsAccounts", len(l.AllProjectsAccounts) > 0},
	} {
		if !in.set {
			return nil, core.InputErrorf(path+"."+in.name, "%s.%s is required with privateProjects: the listener of a hub that may make a project private reads each decision's privacy from the secrets broker (agentSecretsUrl, brokerAudience, privacySourceUrl), checks each caller's proof against its own origin (publicUrl, the tailnet's name with tailnet) and serves every project's sessions to the callers allProjectsAccounts names", path, in.name)
		}
	}
	origin := l.PublicUrl
	if tailnet != nil {
		if l.PublicUrl != "" && strings.TrimSuffix(l.PublicUrl, "/") != tailnet.envoyURL {
			return nil, core.InputErrorf(path+".publicUrl", "%s.publicUrl %q is not %s, the origin the tailnet's sessions reach the listener at: the listener checks every caller's DPoP proof against one origin, so a proof signed for the tailnet's name would be refused; leave publicUrl unset with tailnet",
				path, l.PublicUrl, tailnet.envoyURL)
		}
		origin = tailnet.envoyURL
	}
	if err := core.CheckEnvValue(path+".brokerAudience", "the broker token's audience", l.BrokerAudience); err != nil {
		return nil, err
	}
	if l.BrokerAudience == l.NatsAudience {
		return nil, core.InputErrorf(path+".brokerAudience", "%s.brokerAudience is %s.natsAudience's audience, %q: that token is the auth service's, which the secrets broker's routes do not accept, so the listener could neither introspect a caller's token nor read privacy with it",
			path, path, l.NatsAudience)
	}
	admitted := make(map[string]bool, len(credential.accounts))
	for _, account := range credential.accounts {
		admitted[account] = true
	}
	privacy := &clusteredHubListenerPrivacy{brokerAudience: l.BrokerAudience, origin: origin}
	for i, account := range l.AllProjectsAccounts {
		namespace, _, ok := core.ServiceAccountSubject(account)
		if !ok || !dnsLabel(namespace) {
			return nil, core.InputErrorf(path+".allProjectsAccounts", "%s.allProjectsAccounts[%d] %q must be system:serviceaccount:<namespace>:<name>, a ServiceAccount's subject", path, i, account)
		}
		if !admitted[account] {
			return nil, core.InputErrorf(path+".allProjectsAccounts", "%s.allProjectsAccounts[%d] %q is not in %s.oidcAccounts: the listener verifies a caller's projected token before it serves it every project's sessions", path, i, account, path)
		}
		if !slices.Contains(privacy.namespaces, namespace) {
			privacy.namespaces = append(privacy.namespaces, namespace)
		}
	}
	return privacy, nil
}

// clusteredHubListenerCredentialFor resolves the token check the listener l, declared at path,
// has: nil where it has none, which is a tailnet.listener with no clientNamespaces on a hub
// without privateProjects, reached through the tailnet hosts alone, as it always has been. A
// listener declared as the hub's own listener input, or one with clientNamespaces, requires a
// token of every caller, since its NetworkPolicy alone would let every pod it admits send as any
// session: a projected ServiceAccount token oidcIssuer signed for oidcAudience, whose subject is
// one oidcAccounts lists. Each of the three is then required, by its path, and allowedHosts is
// refused, since a listener that requires a bearer serves /v1 to any Host and reads no
// ENVOY_ALLOWED_HOSTS. A tailnet.listener with no clientNamespaces on a hub with privateProjects
// needs the three too, for Dispatch's principal, which its readiness route knows by the projected
// token, but keeps its open /v1 beside the check (open): a token that is presented is verified,
// and a caller that presents none is served at the tailnet name and allowedHosts as before, until
// the open /v1 is removed. Where the listener has no check, each of the three is refused by its
// path rather than dropped. Which it is follows from the input args selected, the hub's own
// listener or the tailnet entry's, from clientNamespaces and from privateProjects: path names the
// inputs a refusal is about and decides nothing.
//
// The listener refuses at boot half the issuer and audience pair and a subject of any other
// shape (cmd/listener/apiauth.go, resolveListenerOIDCAccounts), so those are refused here first;
// the issuer must also be an https URL with no user, query or fragment, as a cluster's issuer
// is, since the listener reads its keys from it, and none of the three may hold whitespace or a
// control character, which an environment variable's value cannot carry intact. An account list
// may not repeat a subject. A seed in any of them is clusteredHubSeededInput's to refuse.
func clusteredHubListenerCredentialFor(args *ClusteredNatsHubArgs, l *ClusteredHubListenerArgs, path string) (*clusteredHubListenerCredential, error) {
	inputs := []struct {
		name string
		set  bool
	}{
		{path + ".oidcIssuer", l.OidcIssuer != ""},
		{path + ".oidcAudience", l.OidcAudience != ""},
		{path + ".oidcAccounts", len(l.OidcAccounts) > 0},
	}
	// clusteredHubListenerArgs selects args.Listener when it is set, and the tailnet entry's
	// listener otherwise, so l is the tailnet entry's exactly when it is not args.Listener.
	inTailnet := l != args.Listener
	open := inTailnet && len(l.ClientNamespaces) == 0
	if open && !args.PrivateProjects {
		for _, in := range inputs {
			if in.set {
				return nil, core.InputErrorf(in.name, "%s is given on a tailnet.listener with no clientNamespaces, which serves the tailnet hosts with no credential, so it would check nothing; declare the listener as listener, or give it clientNamespaces, to require a token of its callers", in.name)
			}
		}
		return nil, nil
	}
	for _, in := range inputs {
		if in.set {
			continue
		}
		if open {
			return nil, core.InputErrorf(in.name, "%s is required because privateProjects is set: the tailnet.listener keeps its open /v1 for a caller that presents no credential, and checks a projected ServiceAccount token that is presented, issued by %s.oidcIssuer for %s.oidcAudience to a subject %s.oidcAccounts lists, which is how its readiness route knows Dispatch's own account", in.name, path, path, path)
		}
		why := "the listener is the hub's own listener input"
		if inTailnet {
			why = "tailnet.listener has clientNamespaces"
		}
		return nil, core.InputErrorf(in.name, "%s is required because %s: the listener then requires every caller's projected ServiceAccount token, issued by %s.oidcIssuer for %s.oidcAudience to a subject %s.oidcAccounts lists, since its NetworkPolicy alone would let every pod it admits send as any session", in.name, why, path, path, path)
	}
	if len(l.AllowedHosts) > 0 && !open {
		return nil, core.InputErrorf(path+".allowedHosts", "%s.allowedHosts is given on a listener that requires a credential, which serves /v1 to any Host and reads no ENVOY_ALLOWED_HOSTS, so it would do nothing; unset it", path)
	}
	blank := func(value string) bool {
		return strings.ContainsFunc(value, unicode.IsSpace) || strings.ContainsFunc(value, unicode.IsControl)
	}
	if blank(l.OidcIssuer) || !issuerURL(l.OidcIssuer) {
		return nil, core.InputErrorf(path+".oidcIssuer", "%s.oidcIssuer %q must be an https URL with a host and no user, query or fragment, such as https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<cluster>", path, l.OidcIssuer)
	}
	if blank(l.OidcAudience) {
		return nil, core.InputErrorf(path+".oidcAudience", "%s.oidcAudience %q holds whitespace or a control character", path, l.OidcAudience)
	}
	seen := make(map[string]int, len(l.OidcAccounts))
	for i, account := range l.OidcAccounts {
		rest, prefixed := strings.CutPrefix(account, "system:serviceaccount:")
		namespace, name, found := strings.Cut(rest, ":")
		if !prefixed || !found || !dnsLabel(namespace) || !dnsSubdomain(name) {
			return nil, core.InputErrorf(path+".oidcAccounts", "%s.oidcAccounts[%d] %q must be system:serviceaccount:<namespace>:<name>, a ServiceAccount's subject", path, i, account)
		}
		if first, repeated := seen[account]; repeated {
			return nil, core.InputErrorf(path+".oidcAccounts", "%s.oidcAccounts[%d] %q repeats %s.oidcAccounts[%d]", path, i, account, path, first)
		}
		seen[account] = i
	}
	return &clusteredHubListenerCredential{issuer: l.OidcIssuer, audience: l.OidcAudience, accounts: l.OidcAccounts, open: open}, nil
}

// issuerURL is whether raw is an OIDC issuer a verifier can discover: an https URL with a host and
// none of the extras urlCarriesExtras names, an empty query or fragment included, which url.Parse
// reads as none but which is not the issuer a token names.
func issuerURL(raw string) bool {
	issuer, err := url.Parse(raw)
	return err == nil && issuer.Scheme == "https" && issuer.Host != "" && !urlCarriesExtras(raw, issuer)
}

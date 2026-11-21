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
// holds each broker-token caller to the projects it may see (plan C's t-c-8). A listener of an
// earlier release admits every live broker token under DPoP and limits none of them by project,
// so with privateProjects the listener's image must name a release at or after this one
// (checkPrivateModeListenerImage). It is empty until t-c-8's release ships, which sets it, and
// empty refuses every image: no private hub runs a session listener before then. A package-level
// variable, so the tests set it to render the private listener (export_test.go).
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
// and its token check (core.ListenerConfig.Env), credential is the platform-identity token check
// it also requires, nil where its broker settings are its one credential
// (clusteredHubListenerEnv), and brokerAudience the audience of the projected token of its own
// account it introspects callers' broker tokens with. natsAudience is set behind the auth
// service, where the listener presents a projected token for it and the seed's Secret is empty;
// it is empty otherwise, where the pod mounts that Secret. privacy is what the listener of a hub
// that may make a project private reads privacy with, nil on any other hub.
type clusteredHubListener struct {
	image, machineID              string
	seedSecretName, seedSecretKey string
	natsAudience, brokerAudience  string
	clientNamespaces              []string
	cpu, memory                   string
	env                           []string
	credential                    *clusteredHubListenerCredential
	privacy                       *clusteredHubListenerPrivacy
}

// clusteredHubListenerPrivacy is the part of a private hub's listener its environment does not
// say: the namespaces of its all-projects callers, which the listener's NetworkPolicy admits at
// its port and which reach its replicas at the headless Service clusteredHubListenerInstancesName
// names.
type clusteredHubListenerPrivacy struct {
	namespaces []string
}

// clusteredHubListenerCredential is the token check a listener requires: the issuer and audience
// of the projected ServiceAccount tokens it verifies, and the subjects it admits.
type clusteredHubListenerCredential struct {
	issuer, audience string
	accounts         []string
}

// The per-source-address introspection limit a session listener renders with tailnet
// (core.ListenerConfig.IntrospectionsPerSecond, IntrospectionBurst). Every tailnet session reaches
// the listener through the tailnet hosts, so every one of them shares a host's address, and the
// listener's default bucket (five a second, a burst of 20, keyed on the source address) is then one
// bucket per host for the whole tailnet: a session spends one introspection per 5 s answer cache
// while it calls and at least one per 90 s registration heartbeat, so two hosts at the default
// saturate near 2 × 5 × 90 = 900 sessions, and a restart that registers every session again passes
// 40 at once. Every session listener checks its callers' broker tokens from t-e-19 on, on an
// all-public hub as on a private one, so the limit is rendered with tailnet whatever
// privateProjects says. The limit here is the secrets broker's own bound on the listener: the
// broker spends one request of its per-source-address bucket on each POST /v1/oauth/introspect
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
// credential it requires of a caller besides its broker settings is
// clusteredHubListenerCredentialFor's to resolve, and the broker settings themselves
// clusteredHubListenerBroker's: every session listener binds off loopback, where it starts only
// with a credential it verifies, so its broker settings are required on every hub (t-e-19). It
// reads no ENVOY_ALLOWED_HOSTS there, so allowedHosts is refused and none is rendered, and its
// /v1 is closed to a caller that presents nothing, so it renders no
// ENVOY_V1_OPEN_WITH_TOKEN_CHECKS. With tailnet it renders the introspection limit every tailnet
// session's host shares. Its sender limit is core's rule (core.CheckSenderLimit), naming
// <path>.senderMessagesPerMinute or <path>.senderMessageBurst.
//
// Its NATS identity follows authCallout (listenerIdentity): without it, the user the hub renders
// for the listener's seed, from the Secret the pod mounts; with it, the projected token of the
// listener's own ServiceAccount for natsAudience, which the pod reads at
// clusteredHubListenerTokenFile, while the hub renders no user for it. With privateProjects it also
// renders the privacy settings (newClusteredHubListenerPrivacy).
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
	if len(l.AllowedHosts) > 0 {
		return nil, nil, core.InputErrorf(path+".allowedHosts", "%s.allowedHosts is given, but the session listener binds off loopback and requires a credential, so it serves /v1 to any Host and reads no ENVOY_ALLOWED_HOSTS; unset it", path)
	}
	rate, burst, err := core.CheckSenderLimit(path+".", l.SenderMessagesPerMinute, l.SenderMessageBurst)
	if err != nil {
		return nil, nil, err
	}
	credential, err := clusteredHubListenerCredentialFor(args, l, path)
	if err != nil {
		return nil, nil, err
	}
	brokerAudience, origin, err := clusteredHubListenerBroker(l, path, s.tailnet)
	if err != nil {
		return nil, nil, err
	}
	privacy, err := newClusteredHubListenerPrivacy(args, l, path, credential)
	if err != nil {
		return nil, nil, err
	}
	listener := &clusteredHubListener{
		image:            l.Image,
		machineID:        machineID,
		seedSecretName:   l.SeedSecretName,
		seedSecretKey:    l.SeedSecretKey,
		natsAudience:     l.NatsAudience,
		brokerAudience:   brokerAudience,
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
	// The listener serves on every address of its own pod: the tailnet hosts and the pods of
	// clientNamespaces reach it through its cluster Service, which a NetworkPolicy admits their
	// pods alone to, and it admits only the callers that present a credential it verifies: a
	// session's broker token, checked against the broker at agentSecretsUrl as the projected token
	// of its own account mounted at clusteredHubListenerBrokerTokenFile, and the platform identity
	// its token check names, where it has one.
	config := core.ListenerConfig{
		MachineID:               machineID,
		Port:                    core.DefaultListenerPort,
		ListenHost:              "0.0.0.0",
		NATSReplicas:            s.natsReplicas,
		StreamMaxBytes:          s.streamMaxBytes,
		SenderMessagesPerMinute: rate,
		SenderMessageBurst:      burst,
		AgentSecretsURL:         l.AgentSecretsUrl,
		BrokerTokenFile:         clusteredHubListenerBrokerTokenFile,
		PublicURL:               origin,
	}
	if s.tailnet != nil {
		config.IntrospectionsPerSecond, config.IntrospectionBurst = clusteredHubListenerIntrospectionsPerSecond, clusteredHubListenerIntrospectionBurst
	}
	if service {
		config.ServiceTokenFile = clusteredHubListenerTokenFile
	}
	// A private hub's listener reads each decision's privacy from the secrets broker in its
	// required mode, with the same projected token: the privacy settings, from its own inputs and,
	// for the flag, from the hub's privateProjects alone. Any other hub's listener renders none of
	// them, whatever the inputs hold (R9).
	if privacy != nil {
		config.PrivacySourceURL, config.PrivacyRequired, config.AllProjectsAccounts = l.PrivacySourceUrl, args.PrivateProjects, l.AllProjectsAccounts
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

// clusteredHubListenerBroker resolves the listener l's broker settings, declared at path, and
// answers the audience of the projected token of its own account it introspects with
// (brokerAudience) and the one origin it checks every caller's DPoP proof against
// (ENVOY_PUBLIC_URL). The listener binds off loopback, where it starts only with a credential it
// verifies, and a session presents a broker token, so agentSecretsUrl is required on every hub
// (t-e-19): a listener without it would refuse its own start and crash-loop. brokerAudience is
// required with it, since the listener takes the broker settings as one set
// (cmd/listener/brokerauth.go, resolveListenerBrokerConfig), and is refused when it is
// natsAudience's: that token is the auth service's, which the broker's routes do not accept.
//
// The callers that present a broker token to a listener with tailnet are the tailnet's sessions,
// which reach it at the Tailscale Service's name, so its origin is that name's,
// http://<serviceName>.<tailnetDnsName>:9020 (the hub's tailnetEnvoyUrl), whatever publicUrl says:
// an unset publicUrl takes it, and any other value is refused, naming publicUrl, since a proof a
// caller signs for the tailnet name would then be refused. Without tailnet, publicUrl is required,
// the one origin its callers reach it at. The URL's and the origin's shapes are core's rules
// (core.CheckBroker, through core.ListenerConfig.Env), named by path.
func clusteredHubListenerBroker(l *ClusteredHubListenerArgs, path string, tailnet *clusteredHubTailnet) (audience, origin string, err error) {
	if l.AgentSecretsUrl == "" {
		return "", "", core.InputErrorf(path+".agentSecretsUrl", "%s.agentSecretsUrl is required: the session listener binds off loopback, where it starts only with a credential it verifies, and its broker settings, with which it checks the broker token every session presents, are that credential (ENVOY_AGENT_SECRETS_URL, ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE, ENVOY_PUBLIC_URL); without them it refuses its own start", path)
	}
	origin = l.PublicUrl
	if tailnet != nil {
		if l.PublicUrl != "" && strings.TrimSuffix(l.PublicUrl, "/") != tailnet.envoyURL {
			return "", "", core.InputErrorf(path+".publicUrl", "%s.publicUrl %q is not %s, the origin the tailnet's sessions reach the listener at: the listener checks every caller's DPoP proof against one origin, so a proof signed for the tailnet's name would be refused; leave publicUrl unset with tailnet",
				path, l.PublicUrl, tailnet.envoyURL)
		}
		origin = tailnet.envoyURL
	} else if origin == "" {
		return "", "", core.InputErrorf(path+".publicUrl", "%s.publicUrl is required with %s.agentSecretsUrl without tailnet: the listener checks each caller's DPoP proof against the one origin its callers reach it at (ENVOY_PUBLIC_URL)", path, path)
	}
	if l.BrokerAudience == "" {
		return "", "", core.InputErrorf(path+".brokerAudience", "%s.brokerAudience is required with %s.agentSecretsUrl: the listener introspects a caller's broker token as the projected token of its own account for this audience (ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE)", path, path)
	}
	if err := core.CheckEnvValue(path+".brokerAudience", "the broker token's audience", l.BrokerAudience); err != nil {
		return "", "", err
	}
	if l.BrokerAudience == l.NatsAudience {
		return "", "", core.InputErrorf(path+".brokerAudience", "%s.brokerAudience is %s.natsAudience's audience, %q: that token is the auth service's, which the secrets broker's routes do not accept, so the listener could neither introspect a caller's token nor read privacy with it",
			path, path, l.NatsAudience)
	}
	return l.BrokerAudience, origin, nil
}

// newClusteredHubListenerPrivacy resolves what the listener l, declared at path, of a hub with
// privateProjects reads privacy with: nil on a hub without it, where the privacy inputs are read
// nowhere. Its image must name a release that enforces the required mode
// (checkPrivateModeListenerImage). Such a listener has a token check (credential, which
// clusteredHubListenerCredentialFor requires of it), since its readiness route knows Dispatch's
// account by its projected token. privacySourceUrl and allProjectsAccounts are each required, by
// its path; the broker settings it reads privacy with are clusteredHubListenerBroker's, required on
// every hub. Each all-projects caller is a subject the token check admits, since the listener
// verifies a caller before it serves it every project's sessions, and its namespace is admitted at
// the listener's port. The values' shapes are core's rules (core.ListenerConfig.Check,
// core.CheckPrivacySource), named by path.
func newClusteredHubListenerPrivacy(args *ClusteredNatsHubArgs, l *ClusteredHubListenerArgs, path string, credential *clusteredHubListenerCredential) (*clusteredHubListenerPrivacy, error) {
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
		{"privacySourceUrl", l.PrivacySourceUrl != ""},
		{"allProjectsAccounts", len(l.AllProjectsAccounts) > 0},
	} {
		if !in.set {
			return nil, core.InputErrorf(path+"."+in.name, "%s.%s is required with privateProjects: the listener of a hub that may make a project private reads each decision's privacy from the secrets broker (privacySourceUrl) and serves every project's sessions to the callers allProjectsAccounts names", path, in.name)
		}
	}
	admitted := make(map[string]bool, len(credential.accounts))
	for _, account := range credential.accounts {
		admitted[account] = true
	}
	privacy := &clusteredHubListenerPrivacy{}
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

// clusteredHubListenerCredentialFor resolves the platform-identity token check the listener l,
// declared at path, requires of every caller besides its broker settings: nil where it requires
// none, which is a tailnet.listener with no clientNamespaces on a hub without privateProjects,
// reached through the tailnet hosts alone, whose callers present their broker tokens. A listener
// declared as the hub's own listener input, or one with clientNamespaces, requires one, since its
// NetworkPolicy alone would let every pod it admits send as any session: a projected
// ServiceAccount token oidcIssuer signed for oidcAudience, whose subject is one oidcAccounts lists.
// Each of the three is then required, by its path. A tailnet.listener with no clientNamespaces on
// a hub with privateProjects needs the three too, for Dispatch's principal, which its readiness
// route knows by the projected token. Where the listener requires none, each of the three is
// refused by its path rather than dropped. Which it is follows from the input args selected, the
// hub's own listener or the tailnet entry's, from clientNamespaces and from privateProjects: path
// names the inputs a refusal is about and decides nothing.
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
	tailnetOnly := inTailnet && len(l.ClientNamespaces) == 0
	if tailnetOnly && !args.PrivateProjects {
		for _, in := range inputs {
			if in.set {
				return nil, core.InputErrorf(in.name, "%s is given on a tailnet.listener with no clientNamespaces, which the tailnet hosts alone reach and which takes their callers' broker tokens and no platform identity, so it would check nothing; declare the listener as listener, or give it clientNamespaces, to require a token of its callers", in.name)
			}
		}
		return nil, nil
	}
	for _, in := range inputs {
		if in.set {
			continue
		}
		if tailnetOnly {
			return nil, core.InputErrorf(in.name, "%s is required because privateProjects is set: the tailnet.listener checks a projected ServiceAccount token that is presented, issued by %s.oidcIssuer for %s.oidcAudience to a subject %s.oidcAccounts lists, which is how its readiness route knows Dispatch's own account", in.name, path, path, path)
		}
		why := "the listener is the hub's own listener input"
		if inTailnet {
			why = "tailnet.listener has clientNamespaces"
		}
		return nil, core.InputErrorf(in.name, "%s is required because %s: the listener then requires every caller's projected ServiceAccount token, issued by %s.oidcIssuer for %s.oidcAudience to a subject %s.oidcAccounts lists, since its NetworkPolicy alone would let every pod it admits send as any session", in.name, why, path, path, path)
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
	return &clusteredHubListenerCredential{issuer: l.OidcIssuer, audience: l.OidcAudience, accounts: l.OidcAccounts}, nil
}

// issuerURL is whether raw is an OIDC issuer a verifier can discover: an https URL with a host and
// none of the extras urlCarriesExtras names, an empty query or fragment included, which url.Parse
// reads as none but which is not the issuer a token names.
func issuerURL(raw string) bool {
	issuer, err := url.Parse(raw)
	return err == nil && issuer.Scheme == "https" && issuer.Host != "" && !urlCarriesExtras(raw, issuer)
}

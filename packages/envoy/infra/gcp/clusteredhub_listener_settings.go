package gcp

import (
	"net/url"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/infra/core"
)

// clusteredHubListener is the resolved session listener the hub runs as a workload of its own,
// nil without one: the tailnet hosts forward the Tailscale Service's port 9020 to it, and the
// pods of clientNamespaces reach it at its cluster Service. env is its environment but the seed
// and its credential (core.ListenerConfig.Env), and credential is the token check it requires of
// every caller, nil where it serves with none (clusteredHubListenerEnv).
type clusteredHubListener struct {
	image, machineID              string
	seedSecretName, seedSecretKey string
	clientNamespaces              []string
	cpu, memory                   string
	env                           []string
	credential                    *clusteredHubListenerCredential
}

// clusteredHubListenerCredential is the token check a listener requires: the issuer and audience
// of the projected ServiceAccount tokens it verifies, and the subjects it admits.
type clusteredHubListenerCredential struct {
	issuer, audience string
	accounts         []string
}

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
	if l.NKey == "" {
		return nil, nil, core.InputErrorf(path+".nkey", "%s.nkey is required", path)
	}
	if !dnsSubdomain(l.SeedSecretName) {
		return nil, nil, core.InputErrorf(path+".seedSecretName", "%s.seedSecretName %q must be a DNS subdomain, the name of a Secret in namespace %s", path, l.SeedSecretName, s.namespace)
	}
	if !isSecretKey(l.SeedSecretKey) {
		return nil, nil, core.InputErrorf(path+".seedSecretKey", "%s.seedSecretKey %q must match %s", path, l.SeedSecretKey, secretKeyRule)
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
	listener := &clusteredHubListener{
		image:            l.Image,
		machineID:        machineID,
		seedSecretName:   l.SeedSecretName,
		seedSecretKey:    l.SeedSecretKey,
		clientNamespaces: l.ClientNamespaces,
		credential:       credential,
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
	// The names a client reaches a listener that requires no credential by, in a fixed order: the
	// tailnet name the Tailscale Service answers at, then the operator's own. Such a listener is a
	// tailnet.listener with no clientNamespaces, so the list always holds the tailnet name. One
	// that requires a credential serves /v1 to any Host and reads no ENVOY_ALLOWED_HOSTS
	// (cmd/listener/apiauth.go), so it renders none.
	var hosts []string
	if credential == nil {
		hosts = append([]string{core.TailnetHostName(s.tailnet.serviceName, s.tailnet.tailnetDNSName)}, l.AllowedHosts...)
	}
	// The listener serves on every address of its own pod: the tailnet hosts and the pods of
	// clientNamespaces reach it through its cluster Service, which a NetworkPolicy admits their
	// pods alone to. A listener with a credential admits only the callers that present it; the
	// tailnet-only one admits every caller the policy lets through, with no credential.
	if listener.env, err = (core.ListenerConfig{
		MachineID:               machineID,
		Port:                    core.DefaultListenerPort,
		ListenHost:              "0.0.0.0",
		AllowedHosts:            hosts,
		NATSReplicas:            s.natsReplicas,
		StreamMaxBytes:          s.streamMaxBytes,
		SenderMessagesPerMinute: rate,
		SenderMessageBurst:      burst,
	}).Env([]string{s.serviceURL}); err != nil {
		return nil, nil, err
	}
	return listener, &listenerUser{machineID: machineID, nkey: l.NKey}, nil
}

// clusteredHubListenerCredentialFor resolves the token check the listener l, declared at path,
// requires of every caller: nil where it requires none, which is a tailnet.listener with no
// clientNamespaces, reached through the tailnet hosts alone, as it always has been. A listener
// declared as the hub's own listener input, or one with clientNamespaces, requires one, since its
// NetworkPolicy alone would let every pod it admits send as any session: a projected
// ServiceAccount token oidcIssuer signed for oidcAudience, whose subject is one oidcAccounts
// lists. Each of the three is then required, by its path, and allowedHosts is refused, since a
// listener that requires a bearer serves /v1 to any Host and reads no ENVOY_ALLOWED_HOSTS. Where
// the listener requires none, each of the three is refused by its path rather than dropped.
// Which of the two it is follows from the input args selected, the hub's own listener or the
// tailnet entry's, and from clientNamespaces: path names the inputs a refusal is about and decides
// nothing.
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
	if inTailnet && len(l.ClientNamespaces) == 0 {
		for _, in := range inputs {
			if in.set {
				return nil, core.InputErrorf(in.name, "%s is given on a tailnet.listener with no clientNamespaces, which serves the tailnet hosts with no credential, so it would check nothing; declare the listener as listener, or give it clientNamespaces, to require a token of its callers", in.name)
			}
		}
		return nil, nil
	}
	why := "the listener is the hub's own listener input"
	if inTailnet {
		why = "tailnet.listener has clientNamespaces"
	}
	for _, in := range inputs {
		if !in.set {
			return nil, core.InputErrorf(in.name, "%s is required because %s: the listener then requires every caller's projected ServiceAccount token, issued by %s.oidcIssuer for %s.oidcAudience to a subject %s.oidcAccounts lists, since its NetworkPolicy alone would let every pod it admits send as any session", in.name, why, path, path, path)
		}
	}
	if len(l.AllowedHosts) > 0 {
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
	return &clusteredHubListenerCredential{issuer: l.OidcIssuer, audience: l.OidcAudience, accounts: l.OidcAccounts}, nil
}

// issuerURL is whether raw is an OIDC issuer a verifier can discover: an https URL with a host and
// none of the extras urlCarriesExtras names, an empty query or fragment included, which url.Parse
// reads as none but which is not the issuer a token names.
func issuerURL(raw string) bool {
	issuer, err := url.Parse(raw)
	return err == nil && issuer.Scheme == "https" && issuer.Host != "" && !urlCarriesExtras(raw, issuer)
}

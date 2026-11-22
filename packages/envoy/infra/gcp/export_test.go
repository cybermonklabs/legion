package gcp

// CloudConfig is the hub VM's cloud-config document, for the tests outside the package.
type CloudConfig = cloudConfig

// ResolveNatsHub is newSettings' refusal, for gcp_test's refusal table, which needs no engine.
func ResolveNatsHub(args *NatsHubArgs) error {
	_, err := newSettings(args)
	return err
}

// CloudConfigOf returns the cloud-config args give the hub's VM: the document, and the user data
// the resolver rendered from it.
func CloudConfigOf(args *NatsHubArgs) (CloudConfig, string, error) {
	s, err := newSettings(args)
	if err != nil {
		return CloudConfig{}, "", err
	}
	return s.vm.document(), s.userData, nil
}

// NatsHubAuthConf is the authorization args render for the hub's NATS server, for the test that
// holds gcp:ClusteredNatsHub's to the same rendering.
func NatsHubAuthConf(args *NatsHubArgs) (string, error) {
	s, err := newSettings(args)
	if err != nil {
		return "", err
	}
	return s.vm.natsConf, nil
}

// ClusteredNatsHubSettings is the resolved gcp:ClusteredNatsHub configuration the external tests
// assert. Tailnet is nil without tailnet, and Listener without a session listener.
type ClusteredNatsHubSettings struct {
	Name, Namespace, ServiceName, ServiceURL string
	NatsImage, ReloaderImage, ExporterImage  string
	NatsReplicas, StreamMaxBytes             int
	DiskSizeGb, MaxFileStoreGb               int
	NatsCPU, NatsMemory                      string
	StorageClassName, PriorityClassName      string
	ClientNamespaces                         []string
	MonitorNamespaces                        []string
	MetricsNamespaces                        []string
	AuthConf                                 string
	PodSelector                              map[string]string
	Tailnet                                  *ClusteredHubTailnetSettings
	Listener                                 *ClusteredHubListenerSettings
	// RouteSecretName, RouteSecretKey and RouteStage are routeAuthorization's, all empty without
	// it.
	RouteSecretName, RouteSecretKey, RouteStage string
}

// ClusteredHubTailnetSettings is the resolved tailnet entry.
type ClusteredHubTailnetSettings struct {
	ServiceName, TailnetDNSName     string
	Tags                            []string
	Replicas                        int
	TailscaleImage                  string
	TailnetNatsURL, TailnetEnvoyURL string
}

// ClusteredHubListenerSettings is the resolved session listener, its environment included,
// wherever it was declared.
type ClusteredHubListenerSettings struct {
	Image, MachineID              string
	SeedSecretName, SeedSecretKey string
	ClientNamespaces              []string
	CPU, Memory                   string
	Env                           []string
}

// ResolveClusteredNatsHub resolves gcp:ClusteredNatsHub's plain inputs for the external refusal
// and default tests, which need no engine.
func ResolveClusteredNatsHub(args *ClusteredNatsHubArgs) (ClusteredNatsHubSettings, error) {
	s, err := newClusteredHubSettings(args)
	if err != nil {
		return ClusteredNatsHubSettings{}, err
	}
	settings := ClusteredNatsHubSettings{
		Name: s.name, Namespace: s.namespace, ServiceName: s.serviceName, ServiceURL: s.serviceURL,
		NatsImage: s.images.nats, ReloaderImage: s.images.reloader, ExporterImage: s.images.exporter,
		NatsReplicas: s.natsReplicas, StreamMaxBytes: s.streamMaxBytes,
		DiskSizeGb: s.size.diskSizeGb, MaxFileStoreGb: s.size.maxFileStoreGb,
		NatsCPU: s.size.natsCPU, NatsMemory: s.size.natsMemory,
		StorageClassName: s.storageClassName, PriorityClassName: s.priorityClassName,
		ClientNamespaces: s.clientNamespaces, MonitorNamespaces: s.monitorNamespaces, MetricsNamespaces: s.metricsNamespaces,
		AuthConf:    s.authConf,
		PodSelector: clusteredHubSelector(s.name),
	}
	if t := s.tailnet; t != nil {
		settings.Tailnet = &ClusteredHubTailnetSettings{
			ServiceName: t.serviceName, TailnetDNSName: t.tailnetDNSName, Tags: t.tags, Replicas: t.replicas,
			TailscaleImage: t.tailscaleImage, TailnetNatsURL: t.natsURL, TailnetEnvoyURL: t.envoyURL,
		}
	}
	if l := s.listener; l != nil {
		settings.Listener = &ClusteredHubListenerSettings{
			Image: l.image, MachineID: l.machineID, SeedSecretName: l.seedSecretName, SeedSecretKey: l.seedSecretKey,
			ClientNamespaces: l.clientNamespaces, CPU: l.cpu, Memory: l.memory, Env: l.env,
		}
	}
	if r := s.route; r != nil {
		settings.RouteSecretName, settings.RouteSecretKey, settings.RouteStage = r.secretName, r.secretKey, r.stage.String()
	}
	return settings, nil
}

// ClusteredHubRouteStages are routeAuthorization.stage's names, in their order.
var ClusteredHubRouteStages = clusteredHubRouteStageNames[:]

// ClusteredHubListenerNamed is the session listener args resolve, with its refusals naming path in
// place of the input the listener is declared at, for the test that holds the listener's token
// check to where it is declared rather than to the name a refusal gives it: its environment, and
// the token check it requires, its issuer, audience and accounts, each "" or nil for none.
func ClusteredHubListenerNamed(args *ClusteredNatsHubArgs, path string) (env []string, issuer, audience string, accounts []string, err error) {
	s, err := newClusteredHubSettings(args)
	if err != nil {
		return nil, "", "", nil, err
	}
	l, _, err := clusteredHubListenerArgs(args)
	if err != nil {
		return nil, "", "", nil, err
	}
	listener, _, err := newClusteredHubListenerSettings(args, l, path, s)
	if err != nil {
		return nil, "", "", nil, err
	}
	if c := listener.credential; c != nil {
		return listener.env, c.issuer, c.audience, c.accounts, nil
	}
	return listener.env, "", "", nil, nil
}

// RefuseClusteredHubSecret is gcp:ClusteredNatsHub's check of a secret Input once it resolves.
var RefuseClusteredHubSecret = refuseClusteredHubSecret

// ProtectClusteredHubStatefulSet is gcp:ClusteredNatsHub's chart transform, for the test that
// calls it directly: chart children are not registered under the mocks.
var ProtectClusteredHubStatefulSet = protectClusteredHubStatefulSet

// The shutdown budget of each gcp:ClusteredNatsHub workload that stops on a SIGTERM, for the
// tests that hold each within its own pod's grace period: the tailnet host's tailscale container,
// whose preStop hook and containerboot's shutdown run inside that pod's grace, and the session
// listener's own shutdown, which runs inside the listener pod's grace, after its preStop wait.
const (
	TailnetShutdownSeconds              = tailnetShutdownSeconds
	ClusteredHubListenerShutdownSeconds = clusteredHubListenerShutdownSeconds
	ClusteredHubListenerGraceSeconds    = clusteredHubListenerGraceSeconds
)

// DispatchRollingUpdate is the resolved rolling update's bounds, for the tests outside the package.
type DispatchRollingUpdate struct {
	MaxUnavailable int
	MaxSurge       int
}

// DispatchSettings is the resolved Dispatch configuration the external tests assert without
// exporting the component's internal representation. RollingUpdate is nil unless StrategyType is
// RollingUpdate.
type DispatchSettings struct {
	DatabaseVersion    string
	DatabaseTier       string
	DeletionProtection bool
	Namespace          string
	Replicas           int
	StrategyType       string
	RollingUpdate      *DispatchRollingUpdate
	AgentSecretsURL    string
	AppClientID        string
	AppID              string
	AppKeySecretName   string
	AppKeySecretKey    string
	AppCPU             string
	AppMemory          string
	AppEphemeral       string
	ProxyEphemeral     string
	PprofPort          int
	// ServerURL is DISPATCH_SERVER_URL and the url output (serverURL), and OutsideOrigin the
	// second origin as rendered, "" when unset. SignInDomain and SignInWorkspaceDomains are
	// DISPATCH_SIGNIN_DOMAIN and DISPATCH_SIGNIN_WORKSPACE_DOMAINS' entries as rendered.
	ServerURL              string
	OutsideOrigin          string
	SignInDomain           string
	SignInWorkspaceDomains []string
}

// ResolveDispatch resolves Dispatch's plain inputs for the external refusal and default tests.
func ResolveDispatch(args *DispatchArgs) (DispatchSettings, error) {
	s, err := newDispatchSettings(args)
	if err != nil {
		return DispatchSettings{}, err
	}
	strategy := dispatchStrategyFor(s.replicas)
	settings := DispatchSettings{
		DatabaseVersion:        s.databaseVersion,
		DatabaseTier:           s.databaseTier,
		DeletionProtection:     s.deletionProtection,
		Namespace:              s.namespace,
		Replicas:               s.replicas,
		StrategyType:           strategy.strategyType,
		AgentSecretsURL:        s.agentSecretsURL,
		AppClientID:            s.appClientID,
		AppID:                  s.appID,
		AppKeySecretName:       s.appKeySecretName,
		AppKeySecretKey:        s.appKeySecretKey,
		AppCPU:                 s.appResources.appCPU,
		AppMemory:              s.appResources.appMemory,
		AppEphemeral:           s.appResources.appEphemeralStorage,
		ProxyEphemeral:         s.appResources.proxyEphemeralStorage,
		PprofPort:              s.pprofPort,
		ServerURL:              s.serverURL(),
		OutsideOrigin:          s.outsideOrigin,
		SignInDomain:           s.signIn.domain,
		SignInWorkspaceDomains: s.signInWorkspaceDomains,
	}
	if rolling := strategy.rollingUpdate; rolling != nil {
		settings.RollingUpdate = &DispatchRollingUpdate{MaxUnavailable: rolling.maxUnavailable, MaxSurge: rolling.maxSurge}
	}
	return settings, nil
}

// DispatchKubeconfig renders the component's stable exec-plugin kubeconfig for the golden test.
func DispatchKubeconfig(endpoint, ca, name string) string {
	return renderKubeconfig(endpoint, ca, name)
}

// DispatchListenerEnvError resolves the listener container's environment from the one NATS URL,
// for the refusal tests, with the front door's proxy in the default namespace: nil when the URL
// renders an environment.
func DispatchListenerEnvError(hostname, url string) error {
	_, err := dispatchListenerEnv(dispatchSettings{hostname: hostname, namespace: defaultDispatchNamespace}, hostname+"-listener", url)
	return err
}

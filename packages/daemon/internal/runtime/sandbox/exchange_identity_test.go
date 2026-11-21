package sandbox

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// testExchangeAudience is the audience production's broker trades a pod's identity at: deployment
// shared/broker.ExchangeAudience, which the broker sets as BROKER_EXCHANGE_K8S_AUDIENCE
// (dispatch://INFRA-748's R1) and legion.yaml renders as agent_secrets.exchange_audience.
const testExchangeAudience = "legion-broker-exchange"

// The two variables a pod's broker clients read to decide which identity the machine proves and
// where its token is (packages/cipher-client/src/environment.ts, IDENTITY_VAR and
// MACHINE_TOKEN_FILE_VAR).
const (
	identityVariable  = "AGENT_SECRETS_MACHINE_IDENTITY"
	tokenFileVariable = "AGENT_SECRETS_MACHINE_TOKEN_FILE"
)

// exchangeEnrolled is the secrets broker's enrollment with its exchange audience, the one
// testOptions gives every runtime.
func exchangeEnrolled() *AgentSecrets {
	return &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour, ExchangeAudience: testExchangeAudience}
}

// exchangeTokenVolumes are the pod's volumes that project a ServiceAccount token for audience.
func exchangeTokenVolumes(pod corev1.PodSpec, audience string) []corev1.Volume {
	var found []corev1.Volume
	for _, v := range pod.Volumes {
		if v.Projected != nil && slices.ContainsFunc(v.Projected.Sources, func(s corev1.VolumeProjection) bool {
			return s.ServiceAccountToken != nil && s.ServiceAccountToken.Audience == audience
		}) {
			found = append(found, v)
		}
	}
	return found
}

// Every pod a launch builds carries an identity the broker's exchange trades for a broker token of
// its own (runtime.kubernetes.agent_secrets.exchange_audience, which New requires): a projected
// token of its ServiceAccount for that audience, an hour long, in a volume of its own, mounted
// read-only in the container every role of the pod runs in, and each role is told
// AGENT_SECRETS_MACHINE_IDENTITY=kubernetes and AGENT_SECRETS_MACHINE_TOKEN_FILE naming that token,
// beside the enrollment it already holds. An issue pod and the project controller's pod both carry
// it. The root golden pins the issue pod's Sandbox byte for byte; its start command, which the
// golden does not hold, is read here for the two variables.
func TestAnEnrolledPodCarriesAnIdentityItCanTrade(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]runtime.SpawnSpec{
		"the root's issue pod":  rootSpec(t),
		"a worker's issue pod":  workerSpec(t),
		"a Claude worker's pod": claudeWorkerSpec(t),
		"the controller's pod":  controllerSpec(t),
	} {
		t.Run(name, func(t *testing.T) {
			pod := podOf(t, r, spec)
			volumes := exchangeTokenVolumes(pod, testExchangeAudience)
			if len(volumes) != 1 || len(volumes[0].Projected.Sources) != 1 {
				t.Fatalf("the pod's volumes projecting a token for %s are %+v, want exactly one holding that token alone", testExchangeAudience, volumes)
			}
			token := volumes[0].Projected.Sources[0].ServiceAccountToken
			if token.ExpirationSeconds == nil || *token.ExpirationSeconds != 3600 || token.Path == "" {
				t.Fatalf("the exchange token is projected as %+v, want an hour-long token at a path", token)
			}
			worker := workerOf(t, r, spec)
			env := envOf(worker)
			if got := env[identityVariable]; got != "kubernetes" {
				t.Errorf("the role is told %s=%q, want kubernetes", identityVariable, got)
			}
			file, told := env[tokenFileVariable]
			if !told {
				t.Fatalf("the role is told no %s, so its broker client has no token to trade", tokenFileVariable)
			}
			held := mountHolding(worker, file)
			if held == nil || held.Name != volumes[0].Name || !held.ReadOnly || file != held.MountPath+"/"+token.Path {
				t.Errorf("%s=%s lands on mount %+v, want the read-only mount of %s's %s", tokenFileVariable, file, held, volumes[0].Name, token.Path)
			}
			if env["AGENT_SECRETS_URL"] != r.agentSecrets.URL || env["AGENT_SECRETS_KEY_DIR"] != AgentSecretsKeyDir {
				t.Errorf("the role is told AGENT_SECRETS_URL=%q AGENT_SECRETS_KEY_DIR=%q, want the enrollment it holds beside its identity", env["AGENT_SECRETS_URL"], env["AGENT_SECRETS_KEY_DIR"])
			}
			for _, init := range pod.InitContainers {
				if slices.ContainsFunc(init.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == volumes[0].Name }) {
					t.Errorf("init container %s mounts the exchange token, which only the roles trade", init.Name)
				}
			}
		})
	}
	probe := r.probeManifest("legion-probe-test", ImageProbe{Contract: 7}, time.Now().Add(time.Hour)).Spec.PodTemplate.Spec
	if found := exchangeTokenVolumes(probe, testExchangeAudience); len(found) > 0 {
		t.Errorf("the image probe's pod projects %+v, which runs no role that trades", found)
	}
}

// A launch that keeps a pod built before the exchange identity was turned on waits for that pod's
// update round: the pod mounts no exchange token, so its role would have no identity to trade, and
// the Envoy listener takes no shared bearer, so the role would hold no credential for the
// listener at all. listenerCredential refuses the launch with runtime.ErrAwaitingPodRound naming
// the pod, before any start reaches the pod and before anything of it is touched, and PodDrift
// still names the exchange token the pod lacks, which is the drift its round replaces it for.
func TestAKeptPodWithoutTheExchangeTokenWaitsForItsRound(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	loc := g.spawn(spec)
	g.builtWithout(loc.Sandbox.Name, exchangeIdentityVolume)
	r2 := restartedOver(t, g, loc, func(*Options) {})
	if found := exchangeTokenVolumes(g.pod(loc.Sandbox.Name).Spec, testExchangeAudience); len(found) > 0 {
		t.Fatalf("the kept pod projects %+v, so it does carry the identity and the case proves nothing", found)
	}
	before := starts(g)
	g.clearActions()
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	_, err := r2.Resume(g.ctx, &loc, spec)
	wantAwaitingRound(t, g, loc, err, before,
		"pod "+loc.Sandbox.Name+" carries no exchange identity", "the Envoy listener, which takes no shared bearer", "its update round replaces it")
	if drifted := driftOf(t, g.ctx, r2, loc); !slices.ContainsFunc(drifted, func(d string) bool { return strings.HasPrefix(d, agentSecretsExchangeVolume+" ") }) {
		t.Errorf("PodDrift = %q, want the missing exchange token among it, so the update round replaces the pod", drifted)
	}
}

// The exchange identity's names — its two variables, its volume (agent-secrets-exchange) and the
// directory its token is mounted at (/var/run/legion/agent-secrets-exchange) — are Legion's in
// every pod a launch builds, so an operator's pod that takes one of them is refused, naming the
// piece of it that collides.
func TestAnOperatorsPodMayNotTakeTheExchangeNames(t *testing.T) {
	const operatorVolume = "operator-identity"
	emptyDir := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	for name, tc := range map[string]struct {
		pod     Pod
		refused string
	}{
		"its own machine identity": {
			pod:     Pod{Env: map[string]string{identityVariable: "metadata"}},
			refused: "runtime.kubernetes.pod.env sets AGENT_SECRETS_MACHINE_IDENTITY, which Legion's runtime sets in every pod that carries the exchange identity (runtime.kubernetes.agent_secrets.exchange_audience)",
		},
		"its own machine token file": {
			pod:     Pod{Env: map[string]string{tokenFileVariable: "/etc/operator/identity/token"}},
			refused: "runtime.kubernetes.pod.env sets AGENT_SECRETS_MACHINE_TOKEN_FILE, which Legion's runtime sets in every pod that carries the exchange identity (runtime.kubernetes.agent_secrets.exchange_audience)",
		},
		"a volume of the exchange identity's name": {
			pod:     Pod{Volumes: []corev1.Volume{{Name: agentSecretsExchangeVolume, VolumeSource: emptyDir}}},
			refused: "runtime.kubernetes.pod.volumes[0].name agent-secrets-exchange is a volume Legion puts in every pod",
		},
		"a mount where the exchange identity's token is mounted": {
			pod: Pod{
				Volumes:      []corev1.Volume{{Name: operatorVolume, VolumeSource: emptyDir}},
				VolumeMounts: []corev1.VolumeMount{{Name: operatorVolume, MountPath: AgentSecretsExchangeDir}},
			},
			refused: "runtime.kubernetes.pod.volume_mounts[0].mount_path /var/run/legion/agent-secrets-exchange overlaps /var/run/legion/agent-secrets-exchange, which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's",
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			opts.Pod = tc.pod
			if _, err := configure(opts); err == nil || err.Error() != "sandbox runtime: "+tc.refused {
				t.Fatalf("configure = %v, want %q", err, "sandbox runtime: "+tc.refused)
			}
		})
	}
}

// A launch spec may not set the exchange identity's two variables: the runtime tells each role of
// a pod that carries the identity both, and a spec setting either is refused as one setting any
// variable the runtime sets itself (runtime.ValidateSpawnSpec).
func TestALaunchSpecMayNotNameTheExchangeVariables(t *testing.T) {
	r, err := configure(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{identityVariable: "metadata", tokenFileVariable: "/etc/operator/identity/token"} {
		t.Run(name, func(t *testing.T) {
			spec := workerSpec(t)
			spec.Env[name] = value
			if _, err := r.prepare(spec); err == nil || !strings.Contains(err.Error(), "Env sets "+name+", which the runtime sets itself") {
				t.Fatalf("prepare = %v, want the spec refused for setting %s", err, name)
			}
		})
	}
}

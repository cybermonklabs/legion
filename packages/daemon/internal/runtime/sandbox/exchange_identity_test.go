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

// rootEnrolled is the enrollment an agent_secrets block that names only url and token_file settles
// on: the loader's default audience, `cipher`, and its default hour (config's
// TestKubernetesAudienceDefaultsToCipher). The root-enrolled golden pins its pod.
func rootEnrolled() *AgentSecrets {
	return &AgentSecrets{URL: testBrokerURL, Audience: "cipher", TokenExpiry: time.Hour}
}

// exchangeEnrolled is rootEnrolled with the exchange identity turned on.
func exchangeEnrolled() *AgentSecrets {
	enrolled := rootEnrolled()
	enrolled.ExchangeAudience = testExchangeAudience
	return enrolled
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

// An enrolled pod of a deployment that sets agent_secrets.exchange_audience carries an identity the
// broker's exchange trades for a broker token of its own: a projected token of its ServiceAccount
// for that audience, an hour long, in a volume of its own, mounted read-only in the container every
// role of the pod runs in, and each role is told AGENT_SECRETS_MACHINE_IDENTITY=kubernetes and
// AGENT_SECRETS_MACHINE_TOKEN_FILE naming that token, beside the enrollment it already holds. An
// issue pod and the project controller's pod both carry it. The root-exchange-identity golden pins
// the issue pod's Sandbox byte for byte; its start command, which the golden does not hold, is read
// here for the two variables.
func TestAnEnrolledPodCarriesAnIdentityItCanTrade(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	r.agentSecrets = exchangeEnrolled()
	checkGolden(t, "root-exchange-identity", manifestOf(t, r, rootSpec(t)))
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

// With agent_secrets.exchange_audience unset — every deployment before it — no pod carries an
// exchange identity and every Sandbox is the one it was before the key existed (R9): each golden
// of manifestCases, the enrolled root's among them, is byte for byte unchanged, no pod projects a
// ServiceAccount token for any audience but the enrollment's, and no role is told either variable.
// A microVM case's metadata endpoint token (workloadIdentityTokenVolume), for the Workload Identity
// pool's audience, is the endpoint's and no exchange identity
// (TestAMicroVMPodWithMetadataMountsAProjectedTokenNoRoleSees pins it), so it is passed over.
func TestNoIdentityWithoutTheKey(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range manifestCases(t) {
		if tc.agentSecrets != nil && tc.agentSecrets.ExchangeAudience != "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			tc.apply(r)
			checkGolden(t, name, manifestOf(t, r, tc.spec))
			for _, v := range podOf(t, r, tc.spec).Volumes {
				if v.Projected == nil || v.Name == workloadIdentityTokenVolume {
					continue
				}
				for _, s := range v.Projected.Sources {
					if s.ServiceAccountToken != nil && (r.agentSecrets == nil || s.ServiceAccountToken.Audience != r.agentSecrets.Audience) {
						t.Errorf("volume %s projects a token for %q with the key unset", v.Name, s.ServiceAccountToken.Audience)
					}
				}
			}
			env := envOf(workerOf(t, r, tc.spec))
			for _, name := range []string{identityVariable, tokenFileVariable} {
				if value, told := env[name]; told {
					t.Errorf("the role is told %s=%q with the key unset", name, value)
				}
			}
		})
	}
}

// A launch that keeps a pod built before the exchange identity was turned on starts its role as
// that pod was built: the pod mounts no exchange token, so its role is told neither variable, and
// trades nothing until the update round replaces the pod — a broker client told a token file that
// is not there would fail every trade it tries, where one told of no identity keeps the credential
// it had. The enrollment it does carry it is still told of. PodDrift names the missing token as a
// drift the round replaces the pod for, and not as one that breaks the pod (R7 of #5285, spec
// v26): this deployment sets no dispatch_token_broker, so no launch into the pod is refused and
// its roles keep working under the drift, and a round whose read finds work keeps the pod.
// TestAKeptPodWithNoTransitionFileWaitsForItsRound holds the one case whose drift breaks the pod.
func TestAKeptPodWithoutTheExchangeTokenIsToldNoIdentity(t *testing.T) {
	g := newRig(t, nil, withOptions(enrolledWith(keptBrokerURL)))
	spec := workerSpec(t)
	loc := g.spawn(spec)
	r2 := secondRuntime(t, g, func(o *Options) {
		o.AgentSecrets = &AgentSecrets{URL: keptBrokerURL, Audience: "agent-secrets", TokenExpiry: time.Hour, ExchangeAudience: testExchangeAudience}
	})()
	g.connectRunning(workerToken, loc.Sandbox.Generation)
	g.eventually("the tester's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(workerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-tester-g2", resumeSession
	kept, err := r2.Resume(g.ctx, &loc, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if kept.Sandbox.PodUID != loc.Sandbox.PodUID {
		t.Fatalf("Resume replaced the pod; this test is about the pod a launch keeps")
	}
	if found := exchangeTokenVolumes(g.pod(loc.Sandbox.Name).Spec, testExchangeAudience); len(found) > 0 {
		t.Fatalf("the kept pod projects %+v, so it does carry the identity and the case proves nothing", found)
	}
	env := startEnv(lastStart(t, g, workerToken))
	for _, name := range []string{identityVariable, tokenFileVariable} {
		if value, told := env[name]; told {
			t.Errorf("the role's generation is told %s=%q while its pod mounts no exchange token", name, value)
		}
	}
	if env["AGENT_SECRETS_URL"] != keptBrokerURL {
		t.Errorf("the role's generation is told AGENT_SECRETS_URL=%q, want the enrollment its pod carries, %s", env["AGENT_SECRETS_URL"], keptBrokerURL)
	}
	drifted := driftOf(t, g, r2, loc)
	if !slices.ContainsFunc(drifted, func(d string) bool { return strings.HasPrefix(d, agentSecretsExchangeVolume+" ") }) {
		t.Errorf("PodDrift = %q, want the missing exchange token among it, so the update round replaces the pod", drifted)
	}
	if slices.ContainsFunc(drifted, func(d string) bool { return strings.HasPrefix(d, runtime.BreakingDrift) }) {
		t.Errorf("PodDrift = %q, want no reason opening on %q: no launch into the pod is refused without dispatch_token_broker, so its roles keep working under the drift",
			drifted, runtime.BreakingDrift)
	}
}

// exchangeDeployments are the three ways a deployment can stand to the exchange identity: no
// agent_secrets block, a block that names no exchange_audience — every deployment before the key
// existed is one of these two — and a block that names one. exchanges is whether the exchange
// identity's names are Legion's: only in the third.
var exchangeDeployments = []struct {
	name         string
	agentSecrets func() *AgentSecrets
	exchanges    bool
}{
	{"no agent_secrets block", func() *AgentSecrets { return nil }, false},
	{"agent_secrets with no exchange_audience", rootEnrolled, false},
	{"exchange_audience set", exchangeEnrolled, true},
}

// The exchange identity's names — its two variables, its volume (agent-secrets-exchange) and the
// directory its token is mounted at (/var/run/legion/agent-secrets-exchange) — are Legion's only
// where the deployment names an exchange audience. A deployment that names none, with no
// agent_secrets block or with a block that sets no exchange_audience, runs exactly as it did before
// the key existed (R9): an operator's pod that takes one of them is accepted, and the pod a launch
// builds carries it as the operator wrote it. A deployment that names an audience refuses that
// pod, naming the piece of it that collides.
func TestAnOperatorKeepsTheExchangeNamesWhileTheExchangeIsOff(t *testing.T) {
	const operatorVolume = "operator-identity"
	emptyDir := corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	cases := []struct {
		name    string
		pod     Pod
		carried func(pod corev1.PodSpec, worker corev1.Container) bool
		refused string
	}{
		{
			name: "its own machine identity",
			pod:  Pod{Env: map[string]string{identityVariable: "metadata"}},
			carried: func(_ corev1.PodSpec, worker corev1.Container) bool {
				return envOf(worker)[identityVariable] == "metadata"
			},
			refused: "runtime.kubernetes.pod.env sets AGENT_SECRETS_MACHINE_IDENTITY, which Legion's runtime sets in every enrolled pod under runtime.kubernetes.agent_secrets.exchange_audience",
		},
		{
			name: "its own machine token file",
			pod:  Pod{Env: map[string]string{tokenFileVariable: "/etc/operator/identity/token"}},
			carried: func(_ corev1.PodSpec, worker corev1.Container) bool {
				return envOf(worker)[tokenFileVariable] == "/etc/operator/identity/token"
			},
			refused: "runtime.kubernetes.pod.env sets AGENT_SECRETS_MACHINE_TOKEN_FILE, which Legion's runtime sets in every enrolled pod under runtime.kubernetes.agent_secrets.exchange_audience",
		},
		{
			name: "a volume of the exchange identity's name",
			pod:  Pod{Volumes: []corev1.Volume{{Name: agentSecretsExchangeVolume, VolumeSource: emptyDir}}},
			carried: func(pod corev1.PodSpec, _ corev1.Container) bool {
				return slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == agentSecretsExchangeVolume && v.EmptyDir != nil })
			},
			refused: "runtime.kubernetes.pod.volumes[0].name agent-secrets-exchange is a volume Legion puts in every pod",
		},
		{
			name: "a mount where the exchange identity's token is mounted",
			pod: Pod{
				Volumes:      []corev1.Volume{{Name: operatorVolume, VolumeSource: emptyDir}},
				VolumeMounts: []corev1.VolumeMount{{Name: operatorVolume, MountPath: AgentSecretsExchangeDir}},
			},
			carried: func(_ corev1.PodSpec, worker corev1.Container) bool {
				return slices.ContainsFunc(worker.VolumeMounts, func(m corev1.VolumeMount) bool {
					return m.Name == operatorVolume && m.MountPath == AgentSecretsExchangeDir
				})
			},
			refused: "runtime.kubernetes.pod.volume_mounts[0].mount_path /var/run/legion/agent-secrets-exchange overlaps /var/run/legion/agent-secrets-exchange, which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's",
		},
	}
	for _, deployment := range exchangeDeployments {
		for _, tc := range cases {
			t.Run(deployment.name+"/"+tc.name, func(t *testing.T) {
				opts := testOptions()
				opts.AgentSecrets, opts.Pod = deployment.agentSecrets(), tc.pod
				r, err := configure(opts)
				if deployment.exchanges {
					if want := "sandbox runtime: " + tc.refused; err == nil || err.Error() != want {
						t.Fatalf("configure = %v, want %q", err, want)
					}
					return
				}
				if err != nil {
					t.Fatalf("configure = %v, want the operator's pod accepted as it was before the key existed", err)
				}
				spec := workerSpec(t)
				if !tc.carried(podOf(t, r, spec), workerOf(t, r, spec)) {
					t.Errorf("the pod a launch builds does not carry %s as the operator wrote it", tc.name)
				}
			})
		}
	}
}

// A launch spec may set the exchange identity's two variables where the deployment names no
// exchange audience, as every spec could before the key existed (R9): the runtime tells no role
// either, so the role is told the spec's value. Where it names one, the runtime tells each role
// of an enrolled pod both, and a spec setting either is refused as one setting any variable the
// runtime sets itself (runtime.ValidateSpawnSpec).
func TestALaunchSpecMayNameTheExchangeVariablesWhileTheExchangeIsOff(t *testing.T) {
	for _, deployment := range exchangeDeployments {
		for name, value := range map[string]string{identityVariable: "metadata", tokenFileVariable: "/etc/operator/identity/token"} {
			t.Run(deployment.name+"/"+name, func(t *testing.T) {
				opts := testOptions()
				opts.AgentSecrets = deployment.agentSecrets()
				r, err := configure(opts)
				if err != nil {
					t.Fatal(err)
				}
				spec := workerSpec(t)
				spec.Env[name] = value
				if deployment.exchanges {
					if _, err := r.prepare(spec); err == nil || !strings.Contains(err.Error(), "Env sets "+name+", which the runtime sets itself") {
						t.Fatalf("prepare = %v, want the spec refused for setting %s", err, name)
					}
					return
				}
				if got := envOf(workerOf(t, r, spec))[name]; got != value {
					t.Errorf("the role is told %s=%q, want the spec's %q", name, got, value)
				}
			})
		}
	}
}

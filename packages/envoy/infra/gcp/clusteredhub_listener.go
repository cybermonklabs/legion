package gcp

import (
	appsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apps/v1"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	policyv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/policy/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
)

const (
	// clusteredHubListenerPreStopSeconds is the listener's preStop wait, the one gcp:Dispatch's
	// listener takes (sleepPreStop): it holds the pod open while its removal from its Service's
	// endpoints reaches every node's proxy, so a connection a node still forwards is still
	// answered. The 25 s it was existed only to outlast the tailscale container's drain in the
	// same pod; the listener has a pod of its own now.
	clusteredHubListenerPreStopSeconds = 5
	// clusteredHubListenerShutdownSeconds is the longest the session listener takes to stop on its
	// SIGTERM: the HTTP server's 10 s, the role lane's 5 s (a 3 s drain of the lane's own client,
	// and the 2 s receipt window for the forwards its handler handed over, roleLane.drain) and the
	// NATS connection's 13 s drain, run one after another
	// (packages/envoy/cmd/listener/main.go:1611,1652,1659, its sum stated at :1656-1658).
	clusteredHubListenerShutdownSeconds = 10 + 5 + 13
	// clusteredHubListenerGraceSeconds is the listener pod's terminationGracePeriodSeconds: it
	// holds the preStop wait and the listener's own stop (clusteredHubListenerPreStopSeconds +
	// clusteredHubListenerShutdownSeconds, 33 s) with a margin, so the pod is gone before the
	// kubelet's SIGKILL, and it stays under the 135 s bind deadline at which a starting listener
	// gives up waiting for the durable this one holds (packages/envoy/cmd/listener/durable.go).
	clusteredHubListenerGraceSeconds = 35

	// The listener pod's seed volume and where its container mounts it: its NATS seed from the
	// operator's Secret, by reference.
	clusteredHubListenerSeedMount = "/var/run/secrets/envoy"
	clusteredHubListenerSeedFile  = "nats-seed"
)

// clusteredHubListenerEnv is the listener container's environment: the resolver's rendering of
// core.ListenerConfig, in its order, the seed file mounted from the operator's Secret, and the
// flag that lets the API serve /v1 with no credential. No ENVOY_API_TOKEN: what keeps /v1 to its
// callers is the listener's NetworkPolicy, which admits the tailnet entry's pods alone, and the
// listener serves on every address of its pod, where its API refuses to start with neither
// bearer nor OIDC pair unless ENVOY_API_ALLOW_UNAUTHENTICATED is set
// (packages/envoy/cmd/listener/apiauth.go).
func clusteredHubListenerEnv(l clusteredHubListener) corev1.EnvVarArray {
	return append(envVars(l.env),
		valueEnv("NATS_NKEY_SEED_FILE", clusteredHubListenerSeedMount+"/"+clusteredHubListenerSeedFile),
		valueEnv("ENVOY_API_ALLOW_UNAUTHENTICATED", "1"))
}

// clusteredHubListenerSelector is the selector labels of the hub's session listener, the pod the
// tailnet hosts forward the Service's listener port to: what its workload selects, what its
// Service sends to and what its NetworkPolicy fences. They share no value of
// app.kubernetes.io/name or app.kubernetes.io/component with clusteredHubSelector or
// clusteredHubTailnetSelector, so no workload's selector adopts another's pods.
func clusteredHubListenerSelector(release string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "nats-listener",
		"app.kubernetes.io/instance":  release,
		"app.kubernetes.io/component": "listener",
	}
}

// clusteredHubListenerName is the name of the session listener's Deployment, Service,
// ServiceAccount, NetworkPolicy and PodDisruptionBudget.
func clusteredHubListenerName(release string) string {
	return release + "-listener"
}

// clusteredHubListenerPod is the session listener's pod: the listener container alone, with the
// NATS seed mounted from the operator's Secret by reference. It runs as clusteredHubTailnetUID,
// the uid its image's own user has, and stops inside clusteredHubListenerGraceSeconds, which
// holds the preStop wait and the listener's own stop. Its three probes read two routes. Startup
// and liveness are answersProbe, curl -sS of /livez, which the listener answers 204 without
// touching NATS: they are status-blind, so only a listener that answers nothing at all is
// restarted, and a listener whose bus is down keeps its warm caches and repairs itself.
// Readiness is readyProbe, curl -fsS of /healthz, which reads the HTTP status and never the
// body: that status is what tells a starting pod from a serving one, and a pod still waiting for
// its durable, which /healthz answers 200 with a starting body, is ready, so the rolling update
// never waits for the stream.
func clusteredHubListenerPod(s clusteredHubSettings) corev1.PodSpecArgs {
	l := s.tailnet.listener
	pod := corev1.PodSpecArgs{
		ServiceAccountName:            pulumi.String(clusteredHubListenerName(s.name)),
		EnableServiceLinks:            pulumi.Bool(false),
		TerminationGracePeriodSeconds: pulumi.Int(clusteredHubListenerGraceSeconds),
		SecurityContext:               tailnetPodSecurity(clusteredHubTailnetUID),
		Containers: corev1.ContainerArray{corev1.ContainerArgs{
			Name: pulumi.String("listener"), Image: pulumi.String(l.image),
			Env:       clusteredHubListenerEnv(l),
			Resources: resources(l.cpu, l.memory),
			VolumeMounts: corev1.VolumeMountArray{corev1.VolumeMountArgs{
				Name: pulumi.String("nats-seed"), MountPath: pulumi.String(clusteredHubListenerSeedMount), ReadOnly: pulumi.Bool(true),
			}},
			// The pod keeps answering while its removal from the Service reaches every node's
			// proxy, then stops on its SIGTERM; the image (debian:bookworm-slim) carries
			// /bin/sleep.
			Lifecycle: sleepPreStop(clusteredHubListenerPreStopSeconds),
			// Up to two minutes for the first answer, then one minute of no answer at all
			// restarts it (answersProbe says why any answer is alive).
			StartupProbe:    answersProbe(core.DefaultListenerPort, 5, 24),
			LivenessProbe:   answersProbe(core.DefaultListenerPort, 10, 6),
			ReadinessProbe:  readyProbe(core.DefaultListenerPort, 5, 6, 2),
			SecurityContext: tailnetContainerSecurity(),
		}},
		Volumes: corev1.VolumeArray{corev1.VolumeArgs{Name: pulumi.String("nats-seed"), Secret: &corev1.SecretVolumeSourceArgs{
			SecretName: pulumi.String(l.seedSecretName),
			Items: corev1.KeyToPathArray{corev1.KeyToPathArgs{
				Key: pulumi.String(l.seedSecretKey), Path: pulumi.String(clusteredHubListenerSeedFile),
			}},
		}}},
	}
	// One pod, so there is nothing to spread: no topology spread constraint and no affinity.
	if s.priorityClassName != "" {
		pod.PriorityClassName = pulumi.String(s.priorityClassName)
	}
	return pod
}

// newClusteredHubListener registers the session listener, each object the component's child on
// its Kubernetes provider (opts), in the hub's namespace and all under one name
// (clusteredHubListenerName): ServiceAccount, which mounts no token, since the listener calls no
// Kubernetes API; Deployment, which waits on the account and on classRead, the priority class
// read, when there is one; the Service the tailnet hosts forward to; the NetworkPolicy fencing
// that Service's port; and the PodDisruptionBudget.
func newClusteredHubListener(ctx *pulumi.Context, s clusteredHubSettings, classRead []pulumi.Resource, opts ...pulumi.ResourceOption) error {
	name := clusteredHubListenerName(s.name)
	account, err := corev1.NewServiceAccount(ctx, name, &corev1.ServiceAccountArgs{
		Metadata:                     objectMeta(name, s.namespace),
		AutomountServiceAccountToken: pulumi.Bool(false),
	}, opts...)
	if err != nil {
		return err
	}
	pods := pulumi.ToStringMap(clusteredHubListenerSelector(s.name))
	// One pod, and the component writes the count with no input behind it: until
	// cybermonklabs/legion#2404 gives the listener a delivery lease, each further copy waits the
	// listener's durable bind deadline for the subscription the first copy holds, gives up and
	// restarts, for ever. The new pod is surged in before the old one goes, so the rollout is a
	// handover rather than a gap, and the durable the old pod holds is free by the time the new
	// one has waited its deadline out.
	if _, err := appsv1.NewDeployment(ctx, name, &appsv1.DeploymentArgs{
		Metadata: objectMeta(name, s.namespace),
		Spec: appsv1.DeploymentSpecArgs{
			Replicas: pulumi.Int(1),
			Strategy: deploymentStrategy(dispatchStrategy{
				strategyType:  dispatchStrategyRollingUpdate,
				rollingUpdate: &dispatchRollingUpdate{maxUnavailable: 0, maxSurge: 1},
			}),
			Selector: &metav1.LabelSelectorArgs{MatchLabels: pods},
			Template: corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{Labels: pods},
				Spec:     clusteredHubListenerPod(s),
			},
		},
	}, append(opts, pulumi.DependsOn(append([]pulumi.Resource{account}, classRead...)))...); err != nil {
		return err
	}
	// The listener's own cluster Service, which each tailnet host's serve config forwards the
	// Tailscale Service's listener port to (clusteredHubServeConfig).
	if _, err := corev1.NewService(ctx, name, &corev1.ServiceArgs{
		Metadata: objectMeta(name, s.namespace),
		Spec: &corev1.ServiceSpecArgs{
			Selector: pods,
			Ports: corev1.ServicePortArray{corev1.ServicePortArgs{
				Port: pulumi.Int(core.DefaultListenerPort), TargetPort: pulumi.Int(core.DefaultListenerPort),
			}},
		},
	}, opts...); err != nil {
		return err
	}
	// The listener's port from the tailnet hosts alone, which is what lets it serve /v1 with no
	// bearer; every other port of the pod is then admitted to nobody, which is what is wanted.
	if err := networkPolicy(ctx, name, s.namespace, clusteredHubListenerSelector(s.name), networkingv1.NetworkPolicyIngressRuleArray{
		ingressFrom(core.DefaultListenerPort, networkingv1.NetworkPolicyPeerArray{labelPeer(clusteredHubTailnetSelector(s.name))}),
	}, opts...); err != nil {
		return err
	}
	// The workload is one pod, so a budget of one unavailable allows its eviction: a node drain
	// or an Autopilot upgrade moves it rather than stalling on it, and no tighter budget could be
	// met at all.
	_, err = policyv1.NewPodDisruptionBudget(ctx, name, &policyv1.PodDisruptionBudgetArgs{
		Metadata: objectMeta(name, s.namespace),
		Spec: &policyv1.PodDisruptionBudgetSpecArgs{
			MaxUnavailable: pulumi.Int(1),
			Selector:       &metav1.LabelSelectorArgs{MatchLabels: pods},
		},
	}, opts...)
	return err
}

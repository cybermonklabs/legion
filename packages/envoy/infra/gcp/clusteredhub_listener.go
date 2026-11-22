package gcp

import (
	"strconv"
	"strings"

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
	// NATS connection's 13 s drain, run one after another. The listener holds the three as
	// stopHTTPGrace, stopLaneGrace and stopClientDrain, summed as stopBudget
	// (packages/envoy/cmd/listener/main.go); this module is a Go module of its own and cannot
	// import them, so the number is written again here and the two listener pod goldens are held
	// to stopBudget by the listener's own TestListenerPodGracesCoverTheStopBudget.
	clusteredHubListenerShutdownSeconds = 10 + 5 + 13
	// clusteredHubListenerGraceSeconds is the listener pod's terminationGracePeriodSeconds: the
	// preStop wait and the listener's own stop (5 + 28 = 33 s) with the margin listenerGraceMargin
	// names (packages/envoy/cmd/listener/main.go), which is the + 7 below: this module cannot
	// import package main, so the listener's own TestListenerPodGracesCoverTheStopBudget holds
	// each rendered pod's grace, less its preStop wait, less the budget, equal to it. That 28 s is
	// clusteredHubListenerShutdownSeconds above, whose three deadlines (the HTTP stop, the role
	// lane's drain and the NATS drain) are hard, and the process exits after them. So the margin
	// covers the kubelet's own latency between the preStop hook's return and SIGTERM, the
	// container's teardown and a late tick on a loaded node, and not a listener that runs over
	// its own budget. A hung pod holds its durable for the whole grace. The replacement gives up
	// waiting for it at the listener's 135 s bind deadline (durableBindDeadline in
	// packages/envoy/cmd/listener/durable.go), which counts from the replacement's /v1 open, since
	// the listener starts the bind right after it. So what has to fit under that deadline is the
	// replacement's startup probe's lag behind that open (clusteredHubListenerStartupProbe) plus
	// this grace: at 40 s the lag may be up to 95 s, and the probe's 10 s period keeps it at most
	// 10 s. A longer grace costs nothing on a normal stop, because the kubelet proceeds as soon as
	// the container exits.
	clusteredHubListenerGraceSeconds = clusteredHubListenerPreStopSeconds + clusteredHubListenerShutdownSeconds + 7

	// The listener pod's seed volume and where its container mounts it: its NATS seed from the
	// operator's Secret, by reference.
	clusteredHubListenerSeedMount = "/var/run/secrets/envoy"
	clusteredHubListenerSeedFile  = "nats-seed"

	// clusteredHubListenerServedGauge is the gauge whose sample line on the listener's /metrics
	// says its /v1 gate is open and its interest list can be served. The listener's
	// registerListGauges (packages/envoy/cmd/listener/main.go) writes it over the stores
	// openV1Routes publishes right after it opens the gate, and writes no sample before then, nor
	// while GET /v1/interests/ would answer the refilling 503 (before the interest copy's first
	// complete read, or once the copy has not followed its bucket for longer than its stale
	// bound); its # HELP and # TYPE lines are in every scrape. A listener has done so since
	// legion-envoy-v29.15.0; an older one writes a sample from its first scrape, so the startup
	// probe passes there at the listener's first answer, as the /livez probe before it did. This
	// module cannot import package main, so the listener's own
	// TestHubListenerStartupProbePassesOnlyOnceV1Serves holds this name, as the pod golden
	// renders it, to registerListGauges.
	clusteredHubListenerServedGauge = "envoy_active_interests"
)

// clusteredHubListenerEnv is the listener container's environment: the resolver's rendering of
// core.ListenerConfig, in its order, the seed file mounted from the operator's Secret, and then how
// the API admits a caller. No ENVOY_API_TOKEN either way. A listener with a credential renders its
// token check, ENVOY_OIDC_ISSUER, ENVOY_OIDC_AUDIENCE and ENVOY_OIDC_ACCOUNTS, and every caller
// presents a projected ServiceAccount token of a listed subject, whatever the NetworkPolicy
// admits. One without, a tailnet.listener with no clientNamespaces, renders the flag that lets
// the API serve /v1 with no credential: its NetworkPolicy admits the tailnet entry's pods alone,
// and that is what keeps /v1 to its callers, since the listener serves on every address of its
// pod, where its API refuses to start with no bearer unless ENVOY_API_ALLOW_UNAUTHENTICATED is set
// (packages/envoy/cmd/listener/apiauth.go). The two never render together.
func clusteredHubListenerEnv(l *clusteredHubListener) corev1.EnvVarArray {
	env := append(envVars(l.env), valueEnv("NATS_NKEY_SEED_FILE", clusteredHubListenerSeedMount+"/"+clusteredHubListenerSeedFile))
	if c := l.credential; c != nil {
		return append(env, valueEnv("ENVOY_OIDC_ISSUER", c.issuer), valueEnv("ENVOY_OIDC_AUDIENCE", c.audience),
			valueEnv("ENVOY_OIDC_ACCOUNTS", strings.Join(c.accounts, ",")))
	}
	return append(env, valueEnv("ENVOY_API_ALLOW_UNAUTHENTICATED", "1"))
}

// clusteredHubListenerSelector is the selector labels of the hub's session listener, the pod the
// tailnet hosts forward the Service's listener port to and the pods of clientNamespaces dial:
// what its workload selects, what its Service sends to and what its NetworkPolicy fences. They
// share no value of app.kubernetes.io/name or app.kubernetes.io/component with
// clusteredHubSelector or clusteredHubTailnetSelector, so no workload's selector adopts another's
// pods.
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

// clusteredHubListenerStartupProbe is the session listener's startup probe: /bin/sh -c of a scrape
// of the listener's /metrics on the container's loopback port, which passes only when the scrape
// holds the sample line of clusteredHubListenerServedGauge, so only once /v1 serves. The probe
// holds no bearer, and a listener that requires one (clusteredHubListenerEnv) refuses /v1 to it
// in apiAuth before the gate, while every listener serves /metrics to any caller (main's mux, and
// isAPIAuthExemptPath in packages/envoy/cmd/listener/apiauth.go). The pattern is anchored and ends
// in the space before the value, so it matches the sample line and never the gauge's "# HELP" or
// "# TYPE" line. With -f curl prints nothing for an HTTP error, and a pipeline's status is its
// last command's, grep's, in a POSIX shell without pipefail, which dash, the listener image's
// /bin/sh, is; TestClusteredNatsHubListenerStartupProbePassesOnlyOnTheSample runs the rendered
// script under the package's test shell (shellCommand: /bin/sh, or the shell ENVOY_TEST_SHELL
// names). --max-time 5 makes curl give up inside the probe's 6 s timeout. 60 failures 10 s apart
// give /v1 ten minutes to open, after which the kubelet restarts the container.
func clusteredHubListenerStartupProbe(port int) *corev1.ProbeArgs {
	script := "curl -fsS --max-time 5 http://127.0.0.1:" + strconv.Itoa(port) + "/metrics | grep -q '^" + clusteredHubListenerServedGauge + " '"
	return &corev1.ProbeArgs{
		Exec:          &corev1.ExecActionArgs{Command: pulumi.StringArray{pulumi.String("/bin/sh"), pulumi.String("-c"), pulumi.String(script)}},
		PeriodSeconds: pulumi.Int(10), TimeoutSeconds: pulumi.Int(6), FailureThreshold: pulumi.Int(60),
	}
}

// clusteredHubListenerPod is the session listener's pod: the listener container alone, with the
// NATS seed mounted from the operator's Secret by reference. It runs as clusteredHubTailnetUID,
// the uid its image's own user has, and stops inside clusteredHubListenerGraceSeconds, which
// holds the preStop wait and the listener's own stop. Its startup probe
// (clusteredHubListenerStartupProbe) passes only once the listener's /v1 gate is open, and until
// it has passed the kubelet runs neither readiness nor liveness and the pod is not Ready.
// So a new pod stays out of its Service until /v1 serves, and the rolling update
// newClusteredHubListener sets while replaceUnderHold is unset or false (maxSurge 1,
// maxUnavailable 0) keeps the old pod serving until the new one is Ready, which the Deployment
// controller holds. The gate opens without waiting for the durable consumer the old pod holds
// until it stops (main opens /v1 before bindListenerDurable, packages/envoy/cmd/listener/main.go),
// so the new pod's gate cannot wait on the old pod.
// Readiness and liveness are answersProbe, curl -sS of /livez, which the listener answers 204
// without touching NATS: they are status-blind, so a fault of the hub, which every pod shares,
// neither takes the one pod out of its Service nor restarts it, and a listener whose bus is down
// keeps its warm caches and repairs itself; only one that answers nothing at all is taken out or
// restarted.
func clusteredHubListenerPod(s clusteredHubSettings) corev1.PodSpecArgs {
	l := s.listener
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
			// Up to ten minutes for /v1 to open (clusteredHubListenerStartupProbe), then
			// status-blind (answersProbe says why any answer is alive): 30 s of no answer at all
			// takes the pod out of its Service and one minute of it restarts the container.
			StartupProbe:    clusteredHubListenerStartupProbe(core.DefaultListenerPort),
			LivenessProbe:   answersProbe(core.DefaultListenerPort, 10, 6),
			ReadinessProbe:  answersProbe(core.DefaultListenerPort, 10, 3),
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
// read, when there is one; the Service the tailnet hosts forward to and the pods of
// clientNamespaces dial; the NetworkPolicy fencing that Service's port; and the
// PodDisruptionBudget.
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
	// restarts, for ever. With replaceUnderHold unset or false, the new pod is surged in before the
	// old one goes, so the rollout is a handover rather than a gap, and the durable the old pod
	// holds is free by the time the new one has waited its deadline out. Under the writer hold the
	// hold refuses the new pod's NATS connection, so its /v1 never serves and its startup probe
	// (clusteredHubListenerStartupProbe) never passes, and a surge would keep the old pod for as
	// long as the hold lasts: with replaceUnderHold true, which the operator sets for that window,
	// the Deployment is recreated and not waited for instead (heldReplacement).
	meta := objectMeta(name, s.namespace)
	strategy := dispatchStrategy{
		strategyType:  dispatchStrategyRollingUpdate,
		rollingUpdate: &dispatchRollingUpdate{maxUnavailable: 0, maxSurge: 1},
	}
	if s.listener.replaceUnderHold {
		strategy = heldReplacement(meta)
	}
	if _, err := appsv1.NewDeployment(ctx, name, &appsv1.DeploymentArgs{
		Metadata: meta,
		Spec: appsv1.DeploymentSpecArgs{
			Replicas: pulumi.Int(1),
			Strategy: deploymentStrategy(strategy),
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
	// Tailscale Service's listener port to (clusteredHubServeConfig) and the pods of
	// clientNamespaces dial.
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
	// The listener's port from the tailnet hosts and the pods of clientNamespaces alone; every
	// other port of the pod is then admitted to nobody, which is what is wanted. With the tailnet
	// hosts alone this policy is what lets the listener serve /v1 with no credential; with
	// clientNamespaces the listener also requires each caller's token (clusteredHubListenerEnv).
	// Each source is a rule of its own, the tailnet hosts' first, and the resolver refuses a
	// listener with neither, whose policy would admit nobody.
	var ingress networkingv1.NetworkPolicyIngressRuleArray
	if s.tailnet != nil {
		ingress = append(ingress, ingressFrom(core.DefaultListenerPort, networkingv1.NetworkPolicyPeerArray{labelPeer(clusteredHubTailnetSelector(s.name))}))
	}
	if namespaces := s.listener.clientNamespaces; len(namespaces) > 0 {
		ingress = append(ingress, ingressFrom(core.DefaultListenerPort, namespacePeers(namespaces)))
	}
	if err := networkPolicy(ctx, name, s.namespace, clusteredHubListenerSelector(s.name), ingress, opts...); err != nil {
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

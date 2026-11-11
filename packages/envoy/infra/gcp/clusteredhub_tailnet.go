package gcp

import (
	"fmt"
	"strconv"

	appsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/apps/v1"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/meta/v1"
	policyv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/policy/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
)

const (
	// clusteredHubTailnetGraceSeconds is each tailnet host pod's terminationGracePeriodSeconds. It
	// holds the tailscale container's shutdown, its preStop hook and then containerboot's own
	// (tailnetShutdownSeconds, 55 s), with 5 s left for tailscaled to exit on its SIGTERM. The
	// advertiser exits on its SIGTERM at once (tailnetAdvertiserBody's trap), so the pod is gone
	// when the tailscale container is, not at the grace period's SIGKILL.
	clusteredHubTailnetGraceSeconds = 60
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
	// (packages/envoy/cmd/listener/main.go:1225,1263,1270, its sum stated at :1267-1269).
	clusteredHubListenerShutdownSeconds = 10 + 5 + 13

	// clusteredHubTailnetUID is the one uid every container of a tailnet host pod runs as
	// (tailnetPodSecurity says why one), and the uid the listener pod runs as too. It is a
	// non-root uid both images run as: the listener image creates user envoy with `useradd -r` on
	// debian:bookworm-slim (packages/envoy/docker/Dockerfile), the first system uid, 999, and runs
	// as it (USER envoy), and the Tailscale image names no user and runs as root unless told
	// otherwise.
	clusteredHubTailnetUID = 999

	// The tailnet host pod's volumes and where its containers mount them: the state volume the
	// tailscale container, the advertiser and the init container share (tailnetServicePaths), and
	// the serve config from ConfigMap <name>-tailnet. The two seed names below are the listener
	// pod's: its NATS seed from the operator's Secret, by reference.
	clusteredHubTailnetStateMount = "/var/lib/tailnet"
	clusteredHubTailnetServeMount = "/etc/tailscale"
	clusteredHubTailnetServeFile  = "serve.json"
	clusteredHubListenerSeedMount = "/var/run/secrets/envoy"
	clusteredHubListenerSeedFile  = "nats-seed"
	// clusteredHubTailnetAuthKeyKey is the key of Secret <name>-tailnet-authkey holding the auth key.
	clusteredHubTailnetAuthKeyKey = "TS_AUTHKEY"

	// The fixed sizes, request and limit, of the tailnet host pod's Tailscale containers. tailscaled
	// runs in userspace and forwards every tailnet connection to NATS and to the listener through
	// its own network stack, so it gets the room gcp:Dispatch's front door gives it in memory; the
	// advertiser and the init container run a busybox script and one mkdir.
	clusteredHubTailscaleCPU     = "250m"
	clusteredHubTailscaleMemory  = "512Mi"
	clusteredHubAdvertiserCPU    = "50m"
	clusteredHubAdvertiserMemory = "64Mi"
)

// clusteredHubListenerHealthURL is the session listener's /healthz on its own pod's loopback,
// which the listener's startup and liveness probes read. The tailnet hosts' advertiser no longer
// reads it: the listener runs in a pod of its own, and no advertiser can reach that loopback.
var clusteredHubListenerHealthURL = fmt.Sprintf("http://127.0.0.1:%d/healthz", core.DefaultListenerPort)

// clusteredHubTailnetHost is the tailnet hosts' Tailscale Service host: svc:<tailnet.serviceName>,
// joined with the component's auth key and the tags, serving the TCP serve config mounted from
// ConfigMap <name>-tailnet, and advertised on the host's own readiness alone. It names no health
// URL: the session listener is a workload of its own now, so a listener fault no longer takes
// NATS off the tailnet, which it did while every host's advertisement hung on a listener beside
// it.
func clusteredHubTailnetHost(s clusteredHubSettings) tailnetServiceHost {
	return tailnetServiceHost{
		paths:         tailnetServicePaths{mount: clusteredHubTailnetStateMount},
		name:          s.tailnet.serviceName,
		authKeySecret: clusteredHubTailnetName(s.name) + "-authkey",
		authKeyKey:    clusteredHubTailnetAuthKeyKey,
		tags:          s.tailnet.tags,
		serveConfig:   clusteredHubTailnetServeMount + "/" + clusteredHubTailnetServeFile,
		graceSeconds:  clusteredHubTailnetGraceSeconds,
	}
}

// clusteredHubServeConfig is the tailnet hosts' serve config: the Service's NATS port forwarded
// to the hub's client Service, and its listener port to the session listener's own cluster
// Service, which its NetworkPolicy admits these hosts alone to, both as plain TCP.
func clusteredHubServeConfig(s clusteredHubSettings) string {
	return tailnetServeTCP(s.tailnet.serviceName, []tailnetTCPForward{
		{port: natsPort, target: clusterLocal(s.serviceName, s.namespace) + ":" + strconv.Itoa(natsPort)},
		{port: core.DefaultListenerPort, target: clusterLocal(clusteredHubListenerName(s.name), s.namespace) + ":" + strconv.Itoa(core.DefaultListenerPort)},
	})
}

// clusteredHubListenerProbe is the listener's startup and liveness check: any HTTP answer from
// /healthz, within the 5 s its handler answers in (curl's 6 s, the probe's 7 s), passes. Without
// -f curl exits 0 on a 503, so a listener answering 503 while NATS is down or its caches warm is
// alive: it retries by itself, and a restart would only drop its warm caches. Only a listener
// that answers nothing at all is restarted. Readiness is readyProbe, curl -fsS of the same
// /healthz, which reads the HTTP status and never the body: a pod still waiting for its durable,
// which /healthz answers 200 with a starting body, is ready, so the rolling update never waits
// for the stream. Startup and liveness stay status-blind, so a 503 never restarts the pod.
func clusteredHubListenerProbe(periodSeconds, failureThreshold int) *corev1.ProbeArgs {
	return &corev1.ProbeArgs{
		Exec: &corev1.ExecActionArgs{Command: pulumi.ToStringArray([]string{
			"curl", "-sS", "-o", "/dev/null", "--max-time", strconv.Itoa(tailnetHealthTimeoutSeconds),
			clusteredHubListenerHealthURL,
		})},
		PeriodSeconds:    pulumi.Int(periodSeconds),
		TimeoutSeconds:   pulumi.Int(tailnetHealthTimeoutSeconds + 1),
		FailureThreshold: pulumi.Int(failureThreshold),
	}
}

// clusteredHubListenerEnv is the listener container's environment: the resolver's rendering of
// core.ListenerConfig, in its order, the seed file mounted from the operator's Secret, and the
// flag that lets the API serve /v1 with no credential. No ENVOY_API_TOKEN: what keeps /v1 to its
// callers is the listener's NetworkPolicy, which admits the tailnet entry's pods alone, and the
// listener serves on every address of its pod, where its API refuses to start with neither
// bearer nor OIDC pair unless ENVOY_API_ALLOW_UNAUTHENTICATED is set
// (packages/envoy/cmd/listener/apiauth.go).
func clusteredHubListenerEnv(l clusteredHubListener) corev1.EnvVarArray {
	env := append(envVars(l.env), valueEnv("NATS_NKEY_SEED_FILE", clusteredHubListenerSeedMount+"/"+clusteredHubListenerSeedFile))
	return append(env, valueEnv("ENVOY_API_ALLOW_UNAUTHENTICATED", "1"))
}

// clusteredHubTailnetPod is a tailnet host's pod: the tailscale container serving the Service
// and the advertiser deciding when it does, both as the one uid, with the init container that
// creates tailscaled's state directory as that uid before tailscaled starts
// (tailnetServicePaths.stateDir says why). The session listener the Service's port 9020 reaches
// is not here: it is a workload of its own (newClusteredHubListener), which the serve config
// forwards that port to over the network.
func clusteredHubTailnetPod(s clusteredHubSettings) corev1.PodSpecArgs {
	t := s.tailnet
	host := clusteredHubTailnetHost(s)
	name := clusteredHubTailnetName(s.name)
	hosts := clusteredHubTailnetSelector(s.name)
	stateMount := corev1.VolumeMountArgs{Name: pulumi.String("state"), MountPath: pulumi.String(host.paths.mount)}
	spread := func(key string) corev1.TopologySpreadConstraintArgs {
		return corev1.TopologySpreadConstraintArgs{
			TopologyKey: pulumi.String(key), MaxSkew: pulumi.Int(1), WhenUnsatisfiable: pulumi.String("DoNotSchedule"),
			LabelSelector: &metav1.LabelSelectorArgs{MatchLabels: pulumi.ToStringMap(hosts)},
		}
	}
	// One host per zone while there are zones to spare, hard: minDomains stops two hosts sharing a
	// zone while the cluster has nodes in fewer zones than hosts, up to three, and one host per
	// node, so a zone or node outage takes one host and not the Service.
	zone := spread("topology.kubernetes.io/zone")
	zone.MinDomains = pulumi.Int(min(t.replicas, 3))
	pod := corev1.PodSpecArgs{
		ServiceAccountName:            pulumi.String(name),
		EnableServiceLinks:            pulumi.Bool(false),
		TerminationGracePeriodSeconds: pulumi.Int(clusteredHubTailnetGraceSeconds),
		SecurityContext:               tailnetPodSecurity(clusteredHubTailnetUID),
		TopologySpreadConstraints:     corev1.TopologySpreadConstraintArray{zone, spread("kubernetes.io/hostname")},
		InitContainers: corev1.ContainerArray{corev1.ContainerArgs{
			Name: pulumi.String("tailscale-state"), Image: pulumi.String(t.tailscaleImage),
			Command:         pulumi.ToStringArray([]string{"mkdir", "-p", host.paths.stateDir()}),
			Resources:       resources(clusteredHubAdvertiserCPU, clusteredHubAdvertiserMemory),
			VolumeMounts:    corev1.VolumeMountArray{stateMount},
			SecurityContext: tailnetContainerSecurity(),
		}},
		Containers: corev1.ContainerArray{
			corev1.ContainerArgs{
				Name: pulumi.String("tailscale"), Image: pulumi.String(t.tailscaleImage),
				Env:       tailnetServiceEnv(host),
				Resources: resources(clusteredHubTailscaleCPU, clusteredHubTailscaleMemory),
				VolumeMounts: corev1.VolumeMountArray{stateMount, corev1.VolumeMountArgs{
					Name: pulumi.String("serve"), MountPath: pulumi.String(clusteredHubTailnetServeMount), ReadOnly: pulumi.Bool(true),
				}},
				StartupProbe: tailnetServiceStartup(host),
				// The pod is ready only while its node hosts the Service, on the container that
				// serves it (tailnetServiceReadiness says why there), so the rolling update and the
				// budget count serving hosts alone.
				ReadinessProbe:  tailnetServiceReadiness(host),
				LivenessProbe:   tailnetServiceLiveness(host),
				Lifecycle:       tailnetPreStop(host),
				SecurityContext: tailnetContainerSecurity(),
			},
			corev1.ContainerArgs{
				Name: pulumi.String("advertiser"), Image: pulumi.String(t.tailscaleImage),
				Command:         pulumi.ToStringArray([]string{"/bin/sh", "-c", tailnetAdvertiserScript(host)}),
				Resources:       resources(clusteredHubAdvertiserCPU, clusteredHubAdvertiserMemory),
				VolumeMounts:    corev1.VolumeMountArray{stateMount},
				SecurityContext: tailnetContainerSecurity(),
			},
		},
		Volumes: corev1.VolumeArray{
			corev1.VolumeArgs{Name: pulumi.String("state"), EmptyDir: &corev1.EmptyDirVolumeSourceArgs{}},
			corev1.VolumeArgs{Name: pulumi.String("serve"), ConfigMap: &corev1.ConfigMapVolumeSourceArgs{Name: pulumi.String(name)}},
		},
	}
	if s.priorityClassName != "" {
		pod.PriorityClassName = pulumi.String(s.priorityClassName)
	}
	return pod
}

// newClusteredHubTailnetHosts registers the tailnet hosts, each the component's child on its
// Kubernetes provider (opts), in the hub's namespace: Secret <name>-tailnet-authkey holding
// tailnetAuthKey, refused blank or seeded once it resolves and kept secret; ConfigMap
// <name>-tailnet holding the serve config; ServiceAccount <name>-tailnet, which mounts no token,
// since no container calls the Kubernetes API; Deployment <name>-tailnet, whose pods forward the
// Tailscale Service's ports and run no session listener; and its PodDisruptionBudget. The
// Deployment waits on the three it names and on classRead, the priority class read, when there
// is one.
func newClusteredHubTailnetHosts(ctx *pulumi.Context, name string, authKey pulumi.StringInput, s clusteredHubSettings, classRead []pulumi.Resource, opts ...pulumi.ResourceOption) error {
	t := s.tailnet
	tailnetName := clusteredHubTailnetName(s.name)
	host := clusteredHubTailnetHost(s)
	secret, err := corev1.NewSecret(ctx, host.authKeySecret, &corev1.SecretArgs{
		Metadata:   objectMeta(host.authKeySecret, s.namespace),
		StringData: pulumi.StringMap{host.authKeyKey: refuseClusteredHubSecret(authKey, name, "tailnetAuthKey")},
	}, opts...)
	if err != nil {
		return err
	}
	serve, err := corev1.NewConfigMap(ctx, tailnetName, &corev1.ConfigMapArgs{
		Metadata: objectMeta(tailnetName, s.namespace),
		Data:     pulumi.StringMap{clusteredHubTailnetServeFile: pulumi.String(clusteredHubServeConfig(s))},
	}, opts...)
	if err != nil {
		return err
	}
	account, err := corev1.NewServiceAccount(ctx, tailnetName, &corev1.ServiceAccountArgs{
		Metadata:                     objectMeta(tailnetName, s.namespace),
		AutomountServiceAccountToken: pulumi.Bool(false),
	}, opts...)
	if err != nil {
		return err
	}
	pods := pulumi.ToStringMap(clusteredHubTailnetSelector(s.name))
	// A new host is surged in and an old one stopped only once the new one serves the tailnet,
	// so a rollout never takes a serving host away first.
	if _, err := appsv1.NewDeployment(ctx, tailnetName, &appsv1.DeploymentArgs{
		Metadata: objectMeta(tailnetName, s.namespace),
		Spec: appsv1.DeploymentSpecArgs{
			Replicas: pulumi.Int(t.replicas),
			Strategy: deploymentStrategy(dispatchStrategy{
				strategyType:  dispatchStrategyRollingUpdate,
				rollingUpdate: &dispatchRollingUpdate{maxUnavailable: 0, maxSurge: 1},
			}),
			Selector: &metav1.LabelSelectorArgs{MatchLabels: pods},
			Template: corev1.PodTemplateSpecArgs{
				Metadata: &metav1.ObjectMetaArgs{Labels: pods},
				Spec:     clusteredHubTailnetPod(s),
			},
		},
	}, append(opts, pulumi.DependsOn(append([]pulumi.Resource{secret, serve, account}, classRead...)))...); err != nil {
		return err
	}
	// A voluntary eviction takes one host at a time, and only while every other host serves.
	_, err = policyv1.NewPodDisruptionBudget(ctx, tailnetName, &policyv1.PodDisruptionBudgetArgs{
		Metadata: objectMeta(tailnetName, s.namespace),
		Spec: &policyv1.PodDisruptionBudgetSpecArgs{
			MaxUnavailable: pulumi.Int(1),
			Selector:       &metav1.LabelSelectorArgs{MatchLabels: pods},
		},
	}, opts...)
	return err
}

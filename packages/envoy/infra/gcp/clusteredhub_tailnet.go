package gcp

import (
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

	// clusteredHubTailnetUID is the one uid every container of a tailnet host pod runs as
	// (tailnetPodSecurity says why one), and the uid the listener pod runs as too. It is a
	// non-root uid both images run as: the listener image creates user envoy with `useradd -r` on
	// debian:bookworm-slim (packages/envoy/docker/Dockerfile), the first system uid, 999, and runs
	// as it (USER envoy), and the Tailscale image names no user and runs as root unless told
	// otherwise.
	clusteredHubTailnetUID = 999

	// The tailnet host pod's volumes and where its containers mount them: the state volume the
	// tailscale container, the advertiser and the init container share (tailnetServicePaths), and
	// the serve config from ConfigMap <name>-tailnet.
	clusteredHubTailnetStateMount = "/var/lib/tailnet"
	clusteredHubTailnetServeMount = "/etc/tailscale"
	clusteredHubTailnetServeFile  = "serve.json"
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

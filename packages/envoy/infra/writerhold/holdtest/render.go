package holdtest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
)

// ChartNatsConf is nats.conf as the nats chart 2.15.1 renders it into ConfigMap envoy-nats-config
// for the clustered hub, with t-13's route authorization: the route URLs and password as variables
// the pods take from a secretKeyRef on Secret envoy-nats-route. Its server_name is $SERVER_NAME,
// which each pod resolves from its own name, and its route password a secret the operator cannot
// read, so the reader skips the digest equality for every pod, as it must after t-13.
const ChartNatsConf = `{
  include auth.conf;
  "cluster": {
    "authorization": {
      "password": $NATS_ROUTE_PASSWORD,
      "user": "route"
    },
    "name": "envoy-nats",
    "no_advertise": true,
    "port": 6222,
    "routes": $NATS_ROUTE_URLS
  },
  "http_port": 8222,
  "jetstream": {
    "max_file_store": 9Gi,
    "max_memory_store": 0,
    "store_dir": "/data"
  },
  "pid_file": "/var/run/nats/nats.pid",
  "port": 4222,
  "server_name": $SERVER_NAME
}
`

// AuthConf is auth.conf as core renders it for a hub behind keys' auth service: the auth service's
// own user, granted the callout's subjects alone, and the auth callout.
func AuthConf(t testing.TB, keys testnats.CalloutKeys) string {
	t.Helper()
	a := core.Authorization{
		Users: []core.User{{
			Name: "envoy-auth", NKey: keys.ServicePublic,
			Publish:   &core.Rules{Allow: []string{"$SYS._INBOX.>"}},
			Subscribe: &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH"}},
		}},
		Callout: &core.AuthCallout{Issuer: keys.IssuerPublic, XKey: keys.XKeyPublic, ServiceUser: "envoy-auth"},
	}
	text, err := a.Render([]string{"auth.conf of the holdtest stack"})
	if err != nil {
		t.Fatalf("holdtest: render auth.conf: %v", err)
	}
	return text
}

// InstallRender gives the Kube the render the clustered hub runs, as the chart shapes it: the
// StatefulSet's nats container started with --config /etc/nats-config/nats.conf from ConfigMap
// envoy-nats-config, its environment the chart's with t-13's route credentials from Secret
// envoy-nats-route, and the ConfigMap holding ChartNatsConf and AuthConf(keys). The cluster's own
// members run testnats' configurations; the hold reads this render, as it reads the hub's records,
// and binds each restarted pod to it through its revision, its image and the ConfigMap's version.
func (k *Kube) InstallRender(t testing.TB, keys testnats.CalloutKeys) {
	t.Helper()
	ctx := context.Background()
	set, err := k.Client.AppsV1().StatefulSets(Namespace).Get(ctx, StatefulSet, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("holdtest: read the StatefulSet: %v", err)
	}
	set.Spec.Template.Spec = chartPodSpec(len(k.Cluster.Servers))
	if _, err := k.Client.AppsV1().StatefulSets(Namespace).Update(ctx, set, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("holdtest: update the StatefulSet: %v", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: Namespace, ResourceVersion: "4711"},
		Data:       map[string]string{"nats.conf": ChartNatsConf, "auth.conf": AuthConf(t, keys)},
	}
	if _, err := k.Client.CoreV1().ConfigMaps(Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("holdtest: create the ConfigMap: %v", err)
	}
}

// chartPodSpec is the nats chart's pod spec for replicas pods: the nats container and the config
// reloader beside it, the configuration from ConfigMap envoy-nats-config.
func chartPodSpec(replicas int) corev1.PodSpec {
	urls := make([]string, replicas)
	for i := range urls {
		urls[i] = fmt.Sprintf(`"nats://route:$(NATS_ROUTE_PASSWORD)@%s.%s-headless:6222"`, Pod(i), StatefulSet)
	}
	secret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "envoy-nats-route"}, Key: key}}
	}
	configMounts := []corev1.VolumeMount{{Name: "pid", MountPath: "/var/run/nats"}, {Name: "config", MountPath: "/etc/nats-config"}}
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name: Container, Image: Image, Args: []string{"--config", "/etc/nats-config/nats.conf"},
				Env: []corev1.EnvVar{
					{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
					{Name: "SERVER_NAME", Value: "$(POD_NAME)"},
					{Name: "NATS_ROUTE_PASSWORD", ValueFrom: secret("route-1")},
					{Name: "NATS_ROUTE_URLS", Value: "[" + strings.Join(urls, ", ") + "]"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "config", MountPath: "/etc/nats-config"}, {Name: "pid", MountPath: "/var/run/nats"},
					{Name: ClaimTemplate, MountPath: "/data"},
				},
			},
			{
				Name: "reloader", Image: "natsio/nats-server-config-reloader:0.24.0",
				Args:         []string{"-pid", "/var/run/nats/nats.pid", "-config", "/etc/nats-config/nats.conf", "-config", "/etc/nats-config/auth.conf"},
				VolumeMounts: configMounts,
			},
		},
		Volumes: []corev1.Volume{
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: ConfigMap}}}},
			{Name: "pid", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}
}

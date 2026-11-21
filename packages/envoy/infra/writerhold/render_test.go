package writerhold

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
)

// renderImage is the nats image the deployment repository's envoy-cluster pins (Pulumi.prod.yaml natsImage, at
// the deployment repository at 32d9f0fa6a).
const renderImage = "mirror.gcr.io/library/nats:2.15.1-alpine@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6"

// renderRevision is the fixture StatefulSet's status.updateRevision.
const renderRevision = "envoy-nats-6d9c8b7f5"

// renderTestdata is testdata/<name>.
func renderTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(data)
}

// authServiceUser is the auth service's own user as core renders it: an nkey user granted the
// callout's subjects alone, subscribe $SYS.REQ.USER.AUTH and publish under $SYS._INBOX.
func authServiceUser(keys testnats.CalloutKeys) core.User {
	return core.User{
		Name: "envoy-auth", NKey: keys.ServicePublic,
		Publish:   &core.Rules{Allow: []string{"$SYS._INBOX.>"}},
		Subscribe: &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH"}},
	}
}

// renderAuthConf is auth.conf as core.Authorization renders it for the clustered hub (gcp
// clusteredHubSettings' authConf): the auth service's user first, then users, with the callout.
func renderAuthConf(t *testing.T, keys testnats.CalloutKeys, users ...core.User) string {
	t.Helper()
	a := core.Authorization{
		Users:   append([]core.User{authServiceUser(keys)}, users...),
		Callout: &core.AuthCallout{Issuer: keys.IssuerPublic, XKey: keys.XKeyPublic, ServiceUser: "envoy-auth"},
	}
	text, err := a.Render([]string{"auth.conf of the writer hold's render tests"})
	require.NoError(t, err)
	return text
}

// userKey is a new nkey user's public key.
func userKey(t *testing.T) string {
	t.Helper()
	pair, err := nkeys.CreateUser()
	require.NoError(t, err)
	public, err := pair.PublicKey()
	require.NoError(t, err)
	return public
}

// secretEnv is an env value from key of Secret name.
func secretEnv(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
}

// chartEnv is the nats container's env as the chart renders it (POD_NAME from the pod's name,
// SERVER_NAME from it, the hub's GOMEMLIMIT), with t-13's route credentials from Secret
// envoy-nats-route: the password from its key route-1, and the route URLs, which carry it, built
// from it by Kubernetes' $(NAME) expansion. The URLs' shape is this fixture's own reading of t-13
// (plan v62 line 1007), which names the two variables and their secretKeyRef source.
func chartEnv() []corev1.EnvVar {
	urls := make([]string, 3)
	for i := range urls {
		urls[i] = fmt.Sprintf(`"nats://route:$(NATS_ROUTE_PASSWORD)@envoy-nats-%d.envoy-nats-headless:6222"`, i)
	}
	return []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "SERVER_NAME", Value: "$(POD_NAME)"},
		{Name: "GOMEMLIMIT", Value: "1638MiB"},
		{Name: "NATS_ROUTE_PASSWORD", ValueFrom: secretEnv("envoy-nats-route", "route-1")},
		{Name: "NATS_ROUTE_URLS", Value: "[" + strings.Join(urls, ", ") + "]"},
	}
}

// chartStatefulSet is StatefulSet envoy-nats as nats chart 2.15.1 renders it for the hub
// (templates/stateful-set.yaml, helm template with gcp.clusteredHubChartValues' values): the nats
// container started with --config /etc/nats-config/nats.conf, mounted from ConfigMap
// envoy-nats-config, the reloader beside it, and the one JetStream claim template envoy-nats-js.
func chartStatefulSet(image string, env []corev1.EnvVar) *appsv1.StatefulSet {
	replicas := int32(3)
	configMounts := []corev1.VolumeMount{{Name: "pid", MountPath: "/var/run/nats"}, {Name: "config", MountPath: "/etc/nats-config"}}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultStatefulSet, Namespace: DefaultNamespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: "envoy-nats-headless",
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: natsContainer, Image: image, Args: []string{"--config", "/etc/nats-config/nats.conf"}, Env: env,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/etc/nats-config"}, {Name: "pid", MountPath: "/var/run/nats"},
							{Name: "envoy-nats-js", MountPath: "/data"},
						},
					},
					{
						Name: "reloader", Image: "natsio/nats-server-config-reloader:0.24.0",
						Args:         []string{"-pid", "/var/run/nats/nats.pid", "-config", "/etc/nats-config/nats.conf", "-config", "/etc/nats-config/auth.conf"},
						VolumeMounts: configMounts,
					},
				},
				Volumes: []corev1.Volume{
					{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: DefaultConfigMap}}}},
					{Name: "pid", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			}},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "envoy-nats-js"}}},
		},
		Status: appsv1.StatefulSetStatus{CurrentRevision: renderRevision, UpdateRevision: renderRevision},
	}
}

// renderConfigMap is ConfigMap envoy-nats-config holding natsConf and authConf, at
// resourceVersion 4711.
func renderConfigMap(natsConf, authConf string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultConfigMap, Namespace: DefaultNamespace, ResourceVersion: "4711"},
		Data:       map[string]string{"nats.conf": natsConf, "auth.conf": authConf},
	}
}

// renderDisk is the persistent disk under claim i's volume, in the fixture's project and zone.
func renderDisk(i int) DiskRef {
	return DiskRef{Project: "example-infra", Zone: "us-west1-a", Name: fmt.Sprintf("pvc-envoy-nats-js-%d", i)}
}

// claimObjects are the three JetStream claims, each Bound to its volume, and the volumes, each a
// pd.csi.storage.gke.io volume whose handle names renderDisk(i).
func claimObjects() []runtime.Object {
	var out []runtime.Object
	for i := range 3 {
		claim := fmt.Sprintf("envoy-nats-js-envoy-nats-%d", i)
		volume := fmt.Sprintf("pv-envoy-nats-%d", i)
		out = append(out,
			&corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: DefaultNamespace},
				Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volume},
				Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
			},
			&corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: volume},
				Spec: corev1.PersistentVolumeSpec{
					ClaimRef: &corev1.ObjectReference{Namespace: DefaultNamespace, Name: claim},
					PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
						Driver: pdDriver, VolumeHandle: renderDisk(i).String(),
					}},
				},
			},
		)
	}
	return out
}

// fakeDisks is Disks over a table of ids, standing in for compute.disks.get.
type fakeDisks struct {
	mu  sync.Mutex
	ids map[DiskRef]uint64
}

func newFakeDisks() *fakeDisks {
	d := &fakeDisks{ids: map[DiskRef]uint64{}}
	for i := range 3 {
		d.ids[renderDisk(i)] = uint64(7000000000000000000 + i)
	}
	return d
}

func (d *fakeDisks) DiskID(_ context.Context, disk DiskRef) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, ok := d.ids[disk]
	if !ok {
		return 0, fmt.Errorf("googleapi: Error 404: The resource '%s' was not found", disk)
	}
	return id, nil
}

func (d *fakeDisks) set(disk DiskRef, id uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids[disk] = id
}

// renderHub is the clustered hub on a fake API server holding objects.
func renderHub(objects ...runtime.Object) (KubeHub, *fake.Clientset) {
	client := fake.NewClientset(objects...)
	return KubeHub{Client: client, Namespace: DefaultNamespace, StatefulSet: DefaultStatefulSet, ConfigMap: DefaultConfigMap}, client
}

// precondition runs Precondition on a fake hub holding natsConf, authConf and the chart's
// StatefulSet.
func precondition(t *testing.T, natsConf, authConf string) (*Render, error) {
	t.Helper()
	hub, _ := renderHub(renderConfigMap(natsConf, authConf), chartStatefulSet(renderImage, chartEnv()))
	return Precondition(t.Context(), hub)
}

// TestHoldTheRender shows the render check (review H8, §3a step 1 "The render", plan v62 line
// 893): the clustered hub's render, the ConfigMap envoy-nats-config the chart renders and the
// StatefulSet that starts each pod with it, read through the Kubernetes API and parsed with
// nats-server's own conf package, stops the hold naming each account block, subject mapping,
// import and export, and the gateway, leafnodes, unauthorized cluster block and missing callout
// the precondition names. Precondition takes no workflow, so the same render stops every
// workflow's preflight. Positive control: the $G-only render, chart 2.15.1's nats.conf with t-13's
// route authorization and core's auth.conf holding the callout and the auth service's own user,
// passes, and names the two route variables it cannot resolve from the pod spec. Production change
// that would fail it: a check that misses a block, that matches keys with their case, or that does
// not read the included auth.conf.
func TestHoldTheRender(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	natsConf := renderTestdata(t, "render-nats.conf")
	authConf := renderAuthConf(t, keys)

	r, err := precondition(t, natsConf, authConf)
	require.NoError(t, err, "the $G-only render with auth_callout and the auth service's own user")
	require.Equal(t, []string{"envoy-nats-0", "envoy-nats-1", "envoy-nats-2"}, r.Servers)
	for _, pod := range r.Servers {
		require.Equal(t, []string{
			"$NATS_ROUTE_PASSWORD (secretKeyRef envoy-nats-route/route-1)",
			"$NATS_ROUTE_URLS ($(NATS_ROUTE_PASSWORD) of secretKeyRef envoy-nats-route/route-1)",
		}, r.Unresolved[pod], "the route variables %s's render references", pod)
	}
	require.Empty(t, r.Digests, "no digest is the pods' while a variable stays unresolved")

	for _, c := range []struct {
		name, add string
		names     []string
	}{
		{"an account block", "accounts { A: {} }\n", []string{"an accounts block (accounts: A)"}},
		{"an account block spelled with capitals", "Accounts { A: {} }\n", []string{"an accounts block (Accounts: A)"}},
		{"an import and an export", "accounts {\n  A: { imports: [ { stream: { account: B, subject: \"x.>\" } } ] }\n  B: { exports: [ { stream: \"x.>\" } ] }\n}\n",
			[]string{"account A's imports", "account B's exports"}},
		{"a service import", "accounts {\n  A: { imports: [ { service: { account: B, subject: \"$KV.envoy_sessions.>\" } } ] }\n  B: { exports: [ { service: \"$KV.envoy_sessions.>\" } ] }\n}\n",
			[]string{"account A's imports", "account B's exports"}},
		{"a top-level subject mapping", "mappings { \"in.>\": \"$KV.envoy_sessions.>\" }\n", []string{"a subject mapping block (mappings: in.>)"}},
		{"a mapping spelled maps", "maps { \"in.>\": \"out.>\" }\n", []string{"a subject mapping block (maps: in.>)"}},
		{"an account's mapping", "accounts { A: { mappings { \"a\": \"b\" } } }\n", []string{"account A's subject mappings (mappings: a)"}},
		{"an operator's JWT accounts", "operator: \"/etc/nats/operator.jwt\"\nresolver: MEMORY\n",
			[]string{"an account source outside $G (operator)", "an account source outside $G (resolver)"}},
		{"a gateway", "gateway { name: other, port: 7222 }\n", []string{"a gateway block (gateway)"}},
		{"a leafnodes block", "leafnodes { port: 7422 }\n", []string{"a leafnodes block (leafnodes)"}},
		{"a leafnodes block spelled leaf", "leaf { remotes: [ { url: \"nats://elsewhere:7422\" } ] }\n", []string{"a leafnodes block (leaf)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := precondition(t, natsConf, authConf+c.add)
			requireRefusal(t, err, StepPreflight, c.names...)
		})
	}

	t.Run("today's cluster block, with no route authorization", func(t *testing.T) {
		_, err := precondition(t, renderTestdata(t, "render-nats-today.conf"), authConf)
		requireRefusal(t, err, StepPreflight, "a cluster block without route authorization (cluster.authorization with a user and a password)")
	})
	t.Run("route authorization with no password", func(t *testing.T) {
		withoutPassword := strings.Replace(natsConf, `"password": $NATS_ROUTE_PASSWORD,`, "", 1)
		require.NotEqual(t, natsConf, withoutPassword)
		_, err := precondition(t, withoutPassword, authConf)
		requireRefusal(t, err, StepPreflight, "a cluster block without route authorization")
	})
	t.Run("no auth_callout", func(t *testing.T) {
		withoutCallout, err := core.Authorization{Users: []core.User{authServiceUser(keys)}}.Render([]string{"no callout"})
		require.NoError(t, err)
		_, err = precondition(t, natsConf, withoutCallout)
		requireRefusal(t, err, StepPreflight, "no auth_callout in authorization")
	})
	t.Run("no authorization at all", func(t *testing.T) {
		_, err := precondition(t, strings.Replace(natsConf, "include auth.conf;", "", 1), authConf)
		requireRefusal(t, err, StepPreflight, "no authorization block, so no auth_callout")
	})
	t.Run("a callout placing clients outside $G", func(t *testing.T) {
		outside := strings.Replace(authConf, "  auth_callout {\n", "  auth_callout {\n    account: \"OTHER\"\n", 1)
		require.NotEqual(t, authConf, outside)
		_, err := precondition(t, natsConf, outside)
		requireRefusal(t, err, StepPreflight, "auth_callout places its clients in account OTHER, not $G")
	})
	t.Run("an include outside the ConfigMap is UNKNOWN", func(t *testing.T) {
		// The reader's directory is made under TMPDIR, so a file beside it parses: the parse
		// succeeds, and only the check of each value's source file refuses it. In the pod the same
		// include reads a file of the image the operator cannot see.
		scratch := t.TempDir()
		t.Setenv("TMPDIR", scratch)
		require.NoError(t, os.WriteFile(filepath.Join(scratch, "outside.conf"), []byte("max_payload: 1024\n"), 0o600))
		_, err := precondition(t, strings.Replace(natsConf, "include auth.conf;", "include auth.conf;\n  include ../outside.conf;", 1), authConf)
		requireUnknown(t, err, StepPreflight, "the render envoy/envoy-nats-config includes ../outside.conf, which is not a file of the ConfigMap")
	})
	t.Run("a render that does not parse is UNKNOWN", func(t *testing.T) {
		_, err := precondition(t, natsConf+"broken: \"no end\n", authConf)
		requireUnknown(t, err, StepPreflight, "parse the render envoy/envoy-nats-config as envoy-nats-0 reads it")
	})
	t.Run("an include the ConfigMap does not hold is UNKNOWN", func(t *testing.T) {
		_, err := precondition(t, strings.Replace(natsConf, "include auth.conf;", "include auth.conf;\n  include params.conf;", 1), authConf)
		requireUnknown(t, err, StepPreflight, "parse the render envoy/envoy-nats-config as envoy-nats-0 reads it", "params.conf")
	})
}

// TestHoldStaticUsers shows the static-user check (§3a "The only static user", plan v62 line
// 913): the auth service's own user, the one user auth_callout's auth_users lists, does not stop
// the hold, and any other static user or credential does, named by the name core's render writes
// beside its key, as does an auth service user granted more than subscribe $SYS.REQ.USER.AUTH and
// publish under $SYS._INBOX. (core's serviceGrantRefusal rule). Control: the render whose only
// static user is the auth service's, with the callout's grant, passes, and Render.AuthUser is that
// user's public nkey, the authorized_user /connz shows for the auth service (report.go's
// Report.AuthUser). A render whose declared servers each name another auth service user, through a
// variable of the pod spec, stops the hold naming them. Production change that would fail it: a
// check that skips a users entry, counts auth_users as the service, reads a missing permissions
// block as no grant rather than every subject, accepts an entry beyond the callout's subjects, or
// leaves AuthUser unset or set from one server alone.
func TestHoldStaticUsers(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	natsConf := renderTestdata(t, "render-nats.conf")

	r, err := precondition(t, natsConf, renderAuthConf(t, keys))
	require.NoError(t, err, "the auth service's own user alone")
	require.Equal(t, keys.ServicePublic, r.AuthUser, "the auth service's user, as auth_callout's auth_users lists it")

	t.Run("a server of its own auth service user each", func(t *testing.T) {
		// Each pod's POD_NAME is its own name (the chart's fieldRef), so each server's one static
		// user, and the one user its auth_users lists, is another.
		perPod := fmt.Sprintf("authorization {\n  users: [\n    { user: $POD_NAME, password: \"x\", permissions: { publish: { allow: [\"$SYS._INBOX.>\"] }, subscribe: { allow: [\"$SYS.REQ.USER.AUTH\"] } } }\n  ]\n"+
			"  auth_callout {\n    issuer: %q\n    auth_users: [ $POD_NAME ]\n    xkey: %q\n  }\n}\n", keys.IssuerPublic, keys.XKeyPublic)
		_, err := precondition(t, natsConf, perPod)
		requireRefusal(t, err, StepPreflight, "the declared servers' renders name different auth service users: envoy-nats-0, envoy-nats-1, envoy-nats-2")
	})
	t.Run("two authorization blocks", func(t *testing.T) {
		// nats-server applies both, in its map's order, so which auth service user is in force is
		// not known; each block alone would pass.
		authConf := renderAuthConf(t, keys)
		second := strings.Replace(authConf, "authorization {", "Authorization {", 1)
		require.NotEqual(t, authConf, second)
		_, err := precondition(t, natsConf, authConf+second)
		requireRefusal(t, err, StepPreflight, "more than one authorization block (Authorization, authorization)")
	})

	listener := core.User{
		Name: "envoy-listener-hub-a", NKey: userKey(t), HasSeed: true,
		Publish: &core.Rules{Allow: []string{"$KV.envoy_sessions.>"}}, Subscribe: &core.Rules{Allow: []string{"_INBOX.>"}},
	}
	t.Run("a second keyed user", func(t *testing.T) {
		_, err := precondition(t, natsConf, renderAuthConf(t, keys, listener))
		requireRefusal(t, err, StepPreflight,
			"static user envoy-listener-hub-a ("+listener.NKey+") beside the auth service's own",
			"auth_callout's auth_users lists 2 users")
	})

	service := fmt.Sprintf(`{"nkey": %q, "permissions": {"publish": {"allow": ["$SYS._INBOX.>"]}, "subscribe": {"allow": ["$SYS.REQ.USER.AUTH"]}}}`, keys.ServicePublic)
	callout := fmt.Sprintf("  auth_callout {\n    issuer: %q\n    auth_users: [%q]\n    xkey: %q\n  }\n", keys.IssuerPublic, keys.ServicePublic, keys.XKeyPublic)
	handWritten := func(users, extra string) string {
		return "authorization {\n  users: [\n    # envoy-auth\n    " + service + "\n" + users + "  ]\n" + callout + extra + "}\n"
	}
	for _, c := range []struct {
		name, authConf string
		names          []string
	}{
		{"a user with a password, not in auth_users", handWritten("    { user: admin, password: \"s3cret\" }\n", ""), []string{"static user admin beside the auth service's own"}},
		{"a keyed user not in auth_users", handWritten("    # stray\n    { nkey: \""+listener.NKey+"\" }\n", ""), []string{"static user stray (" + listener.NKey + ") beside the auth service's own"}},
		{"the block's own user", handWritten("", "  user: root\n  password: x\n"), []string{"a static user root in authorization.user"}},
		{"a token", handWritten("", "  token: \"t0ken\"\n"), []string{"a static token in authorization.token"}},
		{"a fallback user", handWritten("", "") + "no_auth_user: \"" + keys.ServicePublic + "\"\n", []string{"no_auth_user " + keys.ServicePublic}},
		{"a websocket's own user", handWritten("", "") + "websocket { port: 8080, authorization { user: ws, password: x } }\n", []string{"a static credential in websocket.authorization (user)"}},
		{"an MQTT fallback user", handWritten("", "") + "mqtt { port: 1883, no_auth_user: anyone }\n", []string{"mqtt.no_auth_user anyone"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := precondition(t, natsConf, c.authConf)
			requireRefusal(t, err, StepPreflight, c.names...)
		})
	}

	t.Run("today's six static users and no callout", func(t *testing.T) {
		var users []core.User
		for _, name := range []string{"envoy-listener-hub-a", "envoy-listener-hub-b", "dispatch", "envoy-measure", "envoy-move", "envoy-sessions"} {
			users = append(users, core.User{Name: name, NKey: userKey(t), Publish: &core.Rules{Allow: []string{">"}}, Subscribe: &core.Rules{Allow: []string{">"}}})
		}
		today, err := core.Authorization{Users: users}.Render([]string{"today"})
		require.NoError(t, err)
		_, err = precondition(t, renderTestdata(t, "render-nats-today.conf"), today)
		requireRefusal(t, err, StepPreflight, "no auth_callout in authorization", "static user dispatch (", "the static envoy-move (",
			"a cluster block without route authorization")
	})

	for _, c := range []struct {
		name               string
		publish, subscribe []string
		names              []string
	}{
		{"a publish beyond its replies", []string{"$SYS._INBOX.>", "$JS.API.>"}, []string{"$SYS.REQ.USER.AUTH"},
			[]string{"the auth service's user envoy-auth (" + keys.ServicePublic + ") is granted publish $JS.API.>"}},
		{"the bare reply prefix's parent", []string{"$SYS.>"}, []string{"$SYS.REQ.USER.AUTH"}, []string{"is granted publish $SYS.>"}},
		{"a subscribe beyond the requests", []string{"$SYS._INBOX.>"}, []string{"$SYS.REQ.USER.AUTH", "_INBOX.>"}, []string{"is granted subscribe _INBOX.>"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			wide := authServiceUser(keys)
			wide.Publish, wide.Subscribe = &core.Rules{Allow: c.publish}, &core.Rules{Allow: c.subscribe}
			a := core.Authorization{Users: []core.User{wide}, Callout: &core.AuthCallout{Issuer: keys.IssuerPublic, XKey: keys.XKeyPublic, ServiceUser: "envoy-auth"}}
			text, err := a.Render([]string{"a wide auth service"})
			require.NoError(t, err)
			_, err = precondition(t, natsConf, text)
			requireRefusal(t, err, StepPreflight, c.names...)
		})
	}
	t.Run("a queue group on the requests is the callout's grant", func(t *testing.T) {
		queued := authServiceUser(keys)
		queued.Subscribe = &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH envoy-auth"}}
		a := core.Authorization{Users: []core.User{queued}, Callout: &core.AuthCallout{Issuer: keys.IssuerPublic, XKey: keys.XKeyPublic, ServiceUser: "envoy-auth"}}
		text, err := a.Render([]string{"a queue group"})
		require.NoError(t, err)
		_, err = precondition(t, natsConf, text)
		require.NoError(t, err)
	})
	for _, c := range []struct {
		name, entry string
		names       []string
	}{
		{"no permissions", fmt.Sprintf(`{"nkey": %q}`, keys.ServicePublic), []string{"has no permissions, so it may publish and subscribe to every subject"}},
		{"no publish", fmt.Sprintf(`{"nkey": %q, "permissions": {"subscribe": {"allow": ["$SYS.REQ.USER.AUTH"]}}}`, keys.ServicePublic), []string{"may publish to every subject"}},
		{"an empty allow list", fmt.Sprintf(`{"nkey": %q, "permissions": {"publish": {"allow": []}, "subscribe": {"allow": ["$SYS.REQ.USER.AUTH"]}}}`, keys.ServicePublic), []string{"has no publish allow list"}},
		{"a deny alone", fmt.Sprintf(`{"nkey": %q, "permissions": {"publish": {"deny": ["x"]}, "subscribe": "$SYS.REQ.USER.AUTH"}}`, keys.ServicePublic), []string{"has no publish allow list"}},
		{"allow_responses", fmt.Sprintf(`{"nkey": %q, "permissions": {"publish": ["$SYS._INBOX.>"], "subscribe": ["$SYS.REQ.USER.AUTH"], "allow_responses": true}}`, keys.ServicePublic), []string{"is granted allow_responses"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Replace(handWritten("", ""), service, c.entry, 1)
			_, err := precondition(t, natsConf, text)
			requireRefusal(t, err, StepPreflight, c.names...)
		})
	}
}

// hubMovePublish is the deployment repository's envoy-move grant (envoy-cluster/hub.go:285-296, read at
// 32d9f0fa6a), which reaches every protected stream through STREAM.UPDATE.* and STREAM.DELETE.*.
var hubMovePublish = []string{
	"$JS.API.INFO", "$JS.API.STREAM.NAMES", "$JS.API.STREAM.LIST", "$JS.API.STREAM.INFO.*", "$JS.API.STREAM.UPDATE.*",
	"$JS.API.STREAM.RESTORE.*", "$JS.SNAPSHOT.RESTORE.>", "$JS.API.STREAM.DELETE.*", "$JS.API.CONSUMER.NAMES.*",
	"$JS.API.CONSUMER.LIST.*", "$JS.API.CONSUMER.INFO.*.*",
}

// TestHoldPreflightNoEnvoyMove shows step 1's "No static envoy-move user" (plan v62 line 883): a
// render that still holds the deployment repository's static move user, as core renders it with its grant,
// stops the preflight naming envoy-move. Control: the same render without it passes. Production
// change that would fail it: a check that names users by key alone, or that reads the move user's
// key as the auth service's because auth_users lists it too.
func TestHoldPreflightNoEnvoyMove(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	natsConf := renderTestdata(t, "render-nats.conf")
	move := core.User{
		Name: "envoy-move", NKey: userKey(t),
		Publish: &core.Rules{Allow: hubMovePublish}, Subscribe: &core.Rules{Allow: []string{"_INBOX_envoy_move.>"}},
	}
	withMove := renderAuthConf(t, keys, move)
	require.Contains(t, withMove, "# envoy-move\n", "core names the user beside its key")

	_, err := precondition(t, natsConf, withMove)
	requireRefusal(t, err, StepPreflight, "the static envoy-move ("+move.NKey+") user, which connects past the callout")

	_, err = precondition(t, natsConf, renderAuthConf(t, keys))
	require.NoError(t, err, "the control: the same render without envoy-move")
}

// t11Right is one read t-11's Role and ClusterRole grant the operator (plan v62 line 937): get on
// resource by name in namespace, or on every name when name is "".
type t11Right struct {
	resource, namespace, name string
}

// t11PreconditionRights are t-11's reads of the precondition and of step 1's claims: get on
// ConfigMap envoy-nats-config and StatefulSet envoy-nats, get on the three JetStream claims, and
// get on persistentvolumes, which are cluster-scoped.
func t11PreconditionRights() []t11Right {
	rights := []t11Right{
		{"configmaps", DefaultNamespace, DefaultConfigMap},
		{"statefulsets", DefaultNamespace, DefaultStatefulSet},
		{"persistentvolumes", "", ""},
	}
	for i := range 3 {
		rights = append(rights, t11Right{"persistentvolumeclaims", DefaultNamespace, fmt.Sprintf("envoy-nats-js-envoy-nats-%d", i)})
	}
	return rights
}

// grantOnly makes client answer Forbidden to every request but a get rights grants, as the API
// server's RBAC answers the operator's ServiceAccount, and records each request it answered.
func grantOnly(client *fake.Clientset, rights []t11Right) *[]string {
	var mu sync.Mutex
	asked := &[]string{}
	client.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := ""
		if get, ok := action.(k8stesting.GetAction); ok {
			name = get.GetName()
		}
		resource := action.GetResource()
		mu.Lock()
		*asked = append(*asked, fmt.Sprintf("%s %s %s/%s", action.GetVerb(), resource.Resource, action.GetNamespace(), name))
		mu.Unlock()
		for _, r := range rights {
			if action.GetVerb() == "get" && r.resource == resource.Resource && r.namespace == action.GetNamespace() && (r.name == "" || r.name == name) {
				return false, nil, nil
			}
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: resource.Group, Resource: resource.Resource}, name,
			errors.New("t-11's Role grants the operator no such request"))
	})
	return asked
}

// TestHoldThePreconditionReads shows that the precondition's reads need exactly t-11's read
// rights (plan v62 line 900): under a fake API server that grants the operator only t-11's gets by
// name, the render's ConfigMap and StatefulSet and step 1's claims and volumes are read, and each
// request the reader made is one of those gets. Control: each right removed in turn, its read is
// refused Forbidden and the hold stops as UNKNOWN naming that read. The VM instance's read is
// dropped with the VM hub (clustered hub only). Production change that would fail it: a reader that
// lists, watches or reads anything t-11 does not grant, or that reads a refused read as empty.
func TestHoldThePreconditionReads(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	objects := append([]runtime.Object{
		renderConfigMap(renderTestdata(t, "render-nats.conf"), renderAuthConf(t, keys)),
		chartStatefulSet(renderImage, chartEnv()),
	}, claimObjects()...)
	disks := newFakeDisks()
	run := func(rights []t11Right) (*[]string, error) {
		hub, client := renderHub(objects...)
		asked := grantOnly(client, rights)
		r, err := Precondition(t.Context(), hub)
		if err != nil {
			return asked, err
		}
		_, err = ReadClaims(t.Context(), hub, r, disks)
		return asked, err
	}

	asked, err := run(t11PreconditionRights())
	require.NoError(t, err, "the reads under exactly t-11's rights")
	require.Equal(t, []string{
		"get configmaps envoy/envoy-nats-config",
		"get statefulsets envoy/envoy-nats",
		"get persistentvolumeclaims envoy/envoy-nats-js-envoy-nats-0",
		"get persistentvolumes /pv-envoy-nats-0",
		"get persistentvolumeclaims envoy/envoy-nats-js-envoy-nats-1",
		"get persistentvolumes /pv-envoy-nats-1",
		"get persistentvolumeclaims envoy/envoy-nats-js-envoy-nats-2",
		"get persistentvolumes /pv-envoy-nats-2",
	}, *asked, "every request the reader made")

	for i, right := range t11PreconditionRights() {
		t.Run("without get "+right.resource+" "+right.name, func(t *testing.T) {
			rights := append(append([]t11Right{}, t11PreconditionRights()[:i]...), t11PreconditionRights()[i+1:]...)
			_, err := run(rights)
			what := map[string]string{
				"configmaps":   "read ConfigMap envoy/envoy-nats-config",
				"statefulsets": "read StatefulSet envoy/envoy-nats",
				// Without the ClusterRole, the first claim's volume is the first read refused.
				"persistentvolumes":      "read PersistentVolume pv-envoy-nats-0",
				"persistentvolumeclaims": "read PersistentVolumeClaim envoy/" + right.name,
			}[right.resource]
			unknown := requireUnknown(t, err, StepPreflight, what)
			require.True(t, apierrors.IsForbidden(unknown.Err), "the refusal is the API server's Forbidden: %v", unknown.Err)
		})
	}
}

package writerhold

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/envoy/internal/testnats"
)

// renderImageID is the imageID the kubelet reports for a container started from renderImage: the
// repository with the digest it pulled.
const renderImageID = "mirror.gcr.io/library/nats@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6"

// bindingPod is hub pod i as the API server reports it once it runs: its revision label, and its
// containers' statuses, the nats container's with imageID.
func bindingPod(i int, revision, imageID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("envoy-nats-%d", i), Namespace: DefaultNamespace,
			Labels: map[string]string{appsv1.ControllerRevisionHashLabelKey: revision},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "reloader", ImageID: "docker.io/natsio/nats-server-config-reloader@sha256:" + strings.Repeat("1", 64)},
			{Name: natsContainer, ImageID: imageID},
		}},
	}
}

// readVarz is server's /varz, read through its monitoring port as the hold reads a pod's.
func readVarz(t *testing.T, server *testnats.Server) Varz {
	t.Helper()
	var v Varz
	require.NoError(t, NewHTTPMonitor(server.MonitorURL(), nil).Get(t.Context(), "/varz", &v))
	return v
}

// TestHoldTheConfigurationBinding shows §3a step 4's "It runs the checked binary and configuration"
// for a cluster-hub pod (red team R5, plan v62 line 899), with BuildT10's decision of 2026-11-21
// on line 546. A restarted pod whose image digest, revision hash or /varz version differs from the
// checked one stops the hold, as do a ConfigMap changed since step 1, a StatefulSet whose update
// revision moved, and a claim on another volume or another disk; the pod that matches them all
// passes. The measured part, on a real nats-server: for a render whose every variable resolves from
// the pod spec, the reader's digest of the render as envoy-nats-0 reads it equals that server's
// /varz config_digest, and the control, a render changed by one line, gives another digest, which
// the server then reports once it runs that render. A render with secret-backed variables skips
// only the digest equality, naming them. A server whose config_digest changed while its server_id
// did not, as a reload leaves it, stops the hold naming it; the control, a restart, gives a new
// server_id and passes. The image reference rows: a matching tag and /varz version pass, a /varz
// version other than the tag's stops the hold (the control), and a malformed or digest-less
// reference is UNKNOWN. Dropped: the VM's data disk (clustered hub only), and git_commit (the
// binary's identity is the pod's imageID, slices-context decision 3). Production change that would
// fail it: a binding that skips any of these comparisons, a digest computed without the pod's env,
// or a reference parse that takes a tag or a digest it should not.
func TestHoldTheConfigurationBinding(t *testing.T) {
	ctx := t.Context()
	keys := testnats.NewCalloutKeys(t)
	authConf := renderAuthConf(t, keys)
	measured := renderTestdata(t, "render-measured-nats.conf")
	changed := strings.Replace(measured, `"lame_duck_duration": "30s",`, "\"lame_duck_duration\": \"30s\",\n  \"max_connections\": 1000,", 1)
	require.NotEqual(t, measured, changed)

	objects := func(natsConf, image string, change func([]runtime.Object)) []runtime.Object {
		out := []runtime.Object{renderConfigMap(natsConf, authConf), chartStatefulSet(image, chartEnv())}
		for i := range 3 {
			out = append(out, bindingPod(i, renderRevision, renderImageID))
		}
		out = append(out, claimObjects()...)
		if change != nil {
			change(out)
		}
		return out
	}
	hub, _ := renderHub(objects(measured, renderImage, nil)...)
	disks := newFakeDisks()
	r, err := Precondition(ctx, hub)
	require.NoError(t, err)
	claims, err := ReadClaims(ctx, hub, r, disks)
	require.NoError(t, err)
	want := claims[r.Claim("envoy-nats-0")]
	require.Equal(t, ClaimBinding{Claim: "envoy-nats-js-envoy-nats-0", Volume: "pv-envoy-nats-0", Disk: renderDisk(0), DiskID: "7000000000000000000"}, want)

	// The server runs the render as pod envoy-nats-0 does: nats.conf includes auth.conf from its own
	// directory, and SERVER_NAME is the pod's name, as the chart's env makes it.
	t.Setenv("SERVER_NAME", "envoy-nats-0")
	server, _ := testnats.StartNkeyAuthorizedServer(t, userKey(t))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(server.StoreDir()), "auth.conf"), []byte(authConf), 0o600))
	server.Restart(t, measured)
	varz := readVarz(t, server)
	require.Equal(t, "envoy-nats-0", varz.ServerName)
	require.Equal(t, "2.15.1", varz.Version, "the linked nats-server, the pinned tag's release")

	require.Empty(t, r.Unresolved, "every variable of the measured render resolves from the pod spec")
	require.Equal(t, varz.ConfigDigest, r.Digests["envoy-nats-0"], "the reader's digest and the server's /varz config_digest of the same render")
	require.NotEqual(t, r.Digests["envoy-nats-0"], r.Digests["envoy-nats-1"], "each pod's digest holds its own server_name")
	t.Logf("measured: /varz config_digest %s, reader's digest for envoy-nats-0 %s, for envoy-nats-1 %s", varz.ConfigDigest, r.Digests["envoy-nats-0"], r.Digests["envoy-nats-1"])
	require.NoError(t, r.ProveBinding(ctx, hub, disks, "envoy-nats-0", varz, want), "the pod that runs the checked binary and configuration")

	t.Run("the control: a render changed by one line gives another digest", func(t *testing.T) {
		other, _ := renderHub(objects(changed, renderImage, nil)...)
		rc, err := Precondition(ctx, other)
		require.NoError(t, err)
		require.NotEqual(t, varz.ConfigDigest, rc.Digests["envoy-nats-0"])
		requireRefusal(t, rc.ProveBinding(ctx, other, disks, "envoy-nats-0", varz, want), StepProof,
			"pod envoy-nats-0's server reports config_digest \""+varz.ConfigDigest+"\", not "+rc.Digests["envoy-nats-0"])
		server.Restart(t, changed)
		restarted := readVarz(t, server)
		require.Equal(t, rc.Digests["envoy-nats-0"], restarted.ConfigDigest, "the server restarted on the changed render")
		t.Logf("control: reader's digest of the changed render %s; the server restarted on it reports %s", rc.Digests["envoy-nats-0"], restarted.ConfigDigest)
	})

	t.Run("a reload stops the hold; the control, a restart, does not", func(t *testing.T) {
		before := readVarz(t, server)
		require.NoError(t, server.Reload(t, measured))
		reloaded := readVarz(t, server)
		require.Equal(t, before.ServerID, reloaded.ServerID, "a reload keeps the process")
		require.Equal(t, r.Digests["envoy-nats-0"], reloaded.ConfigDigest, "the reloaded render's digest")
		requireRefusal(t, CheckReload(StepProof, "envoy-nats-0", before, reloaded), StepProof,
			"pod envoy-nats-0's server "+before.ServerID+" reloaded its configuration")
		server.Restart(t, measured)
		restarted := readVarz(t, server)
		require.NotEqual(t, reloaded.ServerID, restarted.ServerID)
		t.Logf("reload: server_id %s kept, config_digest %s became %s; restart: server_id %s", before.ServerID, before.ConfigDigest, reloaded.ConfigDigest, restarted.ServerID)
		require.NoError(t, CheckReload(StepProof, "envoy-nats-0", before, restarted))
	})

	t.Run("a secret-backed variable skips the digest equality alone, naming it", func(t *testing.T) {
		production, _ := renderHub(objects(renderTestdata(t, "render-nats.conf"), renderImage, nil)...)
		rp, err := Precondition(ctx, production)
		require.NoError(t, err)
		require.Empty(t, rp.Digests)
		require.Contains(t, rp.Unresolved["envoy-nats-0"], "$NATS_ROUTE_PASSWORD (secretKeyRef envoy-nats-route/route-1)")
		require.NoError(t, rp.ProveBinding(ctx, production, disks, "envoy-nats-0", varz, want))
		other := varz
		other.Version = "2.15.0"
		requireRefusal(t, rp.ProveBinding(ctx, production, disks, "envoy-nats-0", other, want), StepProof, `reports version "2.15.0"`)
	})

	t.Run("the version: a /varz version other than the tag's stops the hold", func(t *testing.T) {
		older := strings.Replace(renderImage, ":2.15.1-alpine@", ":2.15.0-alpine@", 1)
		other, _ := renderHub(objects(measured, older, nil)...)
		ro, err := Precondition(ctx, other)
		require.NoError(t, err)
		require.Equal(t, "2.15.0", ro.Image.Version)
		requireRefusal(t, ro.ProveBinding(ctx, other, disks, "envoy-nats-0", varz, want), StepProof,
			`pod envoy-nats-0's server reports version "2.15.1", not 2.15.0 of the pinned tag 2.15.0-alpine`)
	})

	t.Run("the production Disks reads compute.disks.get's id", func(t *testing.T) {
		var mu sync.Mutex
		var paths []string
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			paths = append(paths, req.Method+" "+req.URL.Path)
			mu.Unlock()
			switch req.URL.Path {
			case "/compute/v1/projects/example-infra/zones/us-west1-a/disks/pvc-envoy-nats-js-0":
				_, _ = io.WriteString(w, `{"id": "7000000000000000003", "name": "pvc-envoy-nats-js-0"}`)
			case "/compute/v1/projects/example-infra/regions/us-west1/disks/pvc-regional":
				_, _ = io.WriteString(w, `{"id": "7000000000000000004", "name": "pvc-regional"}`)
			default:
				http.Error(w, `{"error": {"code": 404, "message": "not found"}}`, http.StatusNotFound)
			}
		}))
		t.Cleanup(api.Close)
		service, err := compute.NewService(ctx, option.WithEndpoint(api.URL+"/compute/v1/"), option.WithoutAuthentication())
		require.NoError(t, err)
		disks := ComputeDisks{Service: service}
		zonal, ok := parseVolumeHandle("projects/example-infra/zones/us-west1-a/disks/pvc-envoy-nats-js-0")
		require.True(t, ok)
		id, err := disks.DiskID(ctx, zonal)
		require.NoError(t, err)
		require.Equal(t, uint64(7000000000000000003), id)
		regional, ok := parseVolumeHandle("projects/example-infra/regions/us-west1/disks/pvc-regional")
		require.True(t, ok)
		id, err = disks.DiskID(ctx, regional)
		require.NoError(t, err)
		require.Equal(t, uint64(7000000000000000004), id)
		_, err = disks.DiskID(ctx, DiskRef{Project: "example-infra", Zone: "us-west1-a", Name: "absent"})
		require.Error(t, err, "a disk compute.disks.get does not find")
		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, []string{
			"GET /compute/v1/projects/example-infra/zones/us-west1-a/disks/pvc-envoy-nats-js-0",
			"GET /compute/v1/projects/example-infra/regions/us-west1/disks/pvc-regional",
			"GET /compute/v1/projects/example-infra/zones/us-west1-a/disks/absent",
		}, paths, "compute.disks.get and compute.regionDisks.get, and nothing else")
		for _, handle := range []string{"projects/p/zones/z/disks", "projects/p/global/x/disks/d", "zones/z/disks/d", "projects//zones/z/disks/d"} {
			_, ok := parseVolumeHandle(handle)
			require.False(t, ok, "the handle %q", handle)
		}
	})

	for _, c := range []struct {
		name   string
		change func([]runtime.Object)
		names  []string
	}{
		{"another image digest", func(o []runtime.Object) {
			o[2].(*corev1.Pod).Status.ContainerStatuses[1].ImageID = "mirror.gcr.io/library/nats@sha256:" + strings.Repeat("f", 64)
		}, []string{"pod envoy-nats-0 runs image sha256:" + strings.Repeat("f", 64) + ", not the pinned " + renderImage}},
		{"another revision", func(o []runtime.Object) {
			o[2].(*corev1.Pod).Labels[appsv1.ControllerRevisionHashLabelKey] = "envoy-nats-0000000"
		}, []string{`pod envoy-nats-0 runs revision "envoy-nats-0000000", not StatefulSet envoy-nats's update revision ` + renderRevision}},
		{"a StatefulSet changed since step 1", func(o []runtime.Object) {
			o[1].(*appsv1.StatefulSet).Status.UpdateRevision = "envoy-nats-1111111"
			o[2].(*corev1.Pod).Labels[appsv1.ControllerRevisionHashLabelKey] = "envoy-nats-1111111"
		}, []string{"StatefulSet envoy/envoy-nats's update revision is envoy-nats-1111111, not step 1's " + renderRevision}},
		{"a ConfigMap at another resourceVersion", func(o []runtime.Object) {
			o[0].(*corev1.ConfigMap).ResourceVersion = "4712"
		}, []string{"ConfigMap envoy/envoy-nats-config is at resourceVersion 4712, not step 1's 4711"}},
		{"a ConfigMap that holds another render", func(o []runtime.Object) {
			o[0].(*corev1.ConfigMap).Data["nats.conf"] = changed
		}, []string{"ConfigMap envoy/envoy-nats-config no longer holds the checked render"}},
		{"a claim on another volume", func(o []runtime.Object) {
			o[5].(*corev1.PersistentVolumeClaim).Spec.VolumeName = "pv-elsewhere"
			pv := o[6].(*corev1.PersistentVolume)
			pv.Name = "pv-elsewhere"
		}, []string{"claim envoy-nats-js-envoy-nats-0 is bound to volume pv-elsewhere, not pv-envoy-nats-0"}},
		{"a claim whose volume belongs to another claim", func(o []runtime.Object) {
			o[5].(*corev1.PersistentVolumeClaim).Spec.VolumeName = "pv-envoy-nats-1"
		}, []string{"volume pv-envoy-nats-1 of claim envoy/envoy-nats-js-envoy-nats-0 is bound to another claim"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			other, _ := renderHub(objects(measured, renderImage, c.change)...)
			requireRefusal(t, r.ProveBinding(ctx, other, disks, "envoy-nats-0", varz, want), StepProof, c.names...)
		})
	}

	t.Run("a disk made again under its old name", func(t *testing.T) {
		recreated := newFakeDisks()
		recreated.set(renderDisk(0), 7100000000000000000)
		requireRefusal(t, r.ProveBinding(ctx, hub, recreated, "envoy-nats-0", varz, want), StepProof,
			"claim envoy-nats-js-envoy-nats-0's disk "+renderDisk(0).String()+" has id 7100000000000000000, not 7000000000000000000")
	})

	t.Run("a pod whose nats container has not started is UNKNOWN", func(t *testing.T) {
		other, _ := renderHub(objects(measured, renderImage, func(o []runtime.Object) {
			o[2].(*corev1.Pod).Status.ContainerStatuses = o[2].(*corev1.Pod).Status.ContainerStatuses[:1]
		})...)
		requireUnknown(t, r.ProveBinding(ctx, other, disks, "envoy-nats-0", varz, want), StepProof, "pod envoy-nats-0 reports no status of its nats container")
	})

	for _, reference := range []string{
		"mirror.gcr.io/library/nats:2.15.1-alpine",
		"mirror.gcr.io/library/nats@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6",
		"mirror.gcr.io/library/nats:latest@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6",
		"mirror.gcr.io/library/nats:2.15@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6",
		"mirror.gcr.io/library/nats:2.15.1-alpine@sha256:2ae57dfba3063d5e",
		"mirror.gcr.io/library/nats:2.15.1-alpine@sha512:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6",
		"localhost:5000/nats@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6",
	} {
		t.Run("the reference "+reference+" is UNKNOWN", func(t *testing.T) {
			other, _ := renderHub(objects(measured, reference, nil)...)
			_, err := Precondition(ctx, other)
			requireUnknown(t, err, StepPreflight, fmt.Sprintf("the nats image %q", reference))
		})
	}
}

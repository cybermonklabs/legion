package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

var cacheRepo = ghrepo.MustParse("acme/widgets")

// longRepo is a repository whose owner/name is 80 characters: longer than a label value may be.
var longRepo = ghrepo.MustParse(strings.Repeat("o", 39) + "/" + strings.Repeat("r", 40))

// bareRuntime is a runtime configured from testOptions and started on nothing: enough to build a
// manifest a test then stores as the API would hold it.
func bareRuntime(t *testing.T) *Runtime {
	t.Helper()
	return configuredRuntime(t, func(*Options) {})
}

// configuredRuntime is bareRuntime configured from testOptions as edit leaves them: the runtime
// whose manifests an earlier configuration of the daemon wrote.
func configuredRuntime(t *testing.T, edit func(*Options)) *Runtime {
	t.Helper()
	opts := testOptions()
	edit(&opts)
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// cacheObject is repo's cache Sandbox as the API would hold it after the runtime made it, with uid,
// in mode, annotated with annotated's repository (repo's own unless the test names another).
func cacheObject(t *testing.T, repo ghrepo.Repository, uid, mode, annotated string) *unstructured.Unstructured {
	t.Helper()
	return cacheObjectOf(t, bareRuntime(t), repo, uid, mode, annotated)
}

// cacheObjectOf is cacheObject as the runtime r made it: its pod template is the one r writes, so a
// test that configures r writes the Sandbox an earlier configuration left.
func cacheObjectOf(t *testing.T, r *Runtime, repo ghrepo.Repository, uid, mode, annotated string) *unstructured.Unstructured {
	t.Helper()
	s := r.cacheManifest(repo)
	s.UID, s.ResourceVersion, s.Generation = types.UID(uid), "1", 2
	s.CreationTimestamp = metav1.NewTime(time.Date(2026, 9, 23, 11, 0, 0, 0, time.UTC))
	s.Spec.OperatingMode = mode
	if annotated != "" {
		s.Annotations[annotationCacheRepo] = annotated
	}
	u, err := encodeSandbox(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// cachePod is the pod of the cache Sandbox with ownerUID, running image, in phase. A cache pod
// mounts no providers Secret, so its spec holds the cache container alone.
func cachePod(repo ghrepo.Repository, uid, ownerUID, image string, phase corev1.PodPhase) *corev1.Pod {
	name := CacheSandboxName(testProject, repo)
	pod := podObject(name, uid, ownerUID, map[string]string{labelProject: testProject, labelCache: cacheMarker}, corev1.PodStatus{Phase: phase})
	pod.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: cacheContainer, Image: image}}}
	return pod
}

// logBuffer is a log the runtime writes while a test reads it.
type logBuffer = testlog.Buffer

// withLog has the runtime log at info and above to buffer.
func withLog(buffer *logBuffer) rigOption {
	return withOptions(func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
}

// sandboxWrites are the writes to Sandboxes and Secrets, in order, as "verb resource name".
func (g *rig) sandboxWrites() []string {
	var out []string
	for _, w := range g.writes() {
		if w.resource == "sandboxes" || w.resource == "secrets" {
			out = append(out, w.String())
		}
	}
	return out
}

// templatePatch is the pod template a JSON patch body sets, nil when it sets none.
func templatePatch(t *testing.T, body string) *podTemplate {
	t.Helper()
	if isMergePatch(body) {
		return nil
	}
	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("patch %q: %v", body, err)
	}
	for _, op := range ops {
		if op.Path == "/spec/podTemplate" {
			var template podTemplate
			if err := json.Unmarshal(op.Value, &template); err != nil {
				t.Fatal(err)
			}
			return &template
		}
	}
	return nil
}

// A cache's repository is its annotation, exactly as GitHub names it, on the Sandbox, its pod
// template and its Secret; its labels carry only a marker, valid label values all; and its name is
// derived from the repository, never parsed back: at most 63 characters, a DNS label ending in eight
// hex digits of the repository's hash, so two repositories whose slugs coincide get two names.
func TestCacheIdentityIsTheAnnotationAndTheNameIsNeverParsed(t *testing.T) {
	t.Parallel()
	r := bareRuntime(t)
	dnsLabel := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, repo := range []ghrepo.Repository{cacheRepo, longRepo, ghrepo.MustParse("acme/widgets-x"), ghrepo.MustParse("acme-widgets/x")} {
		name := CacheSandboxName(testProject, repo)
		if len(name) > maxNameLength || !dnsLabel.MatchString(name) || !regexp.MustCompile(`-[0-9a-f]{8}$`).MatchString(name) {
			t.Errorf("CacheSandboxName(%s) = %q (%d characters), want a DNS label of at most 63 ending in -<8 hex>", repo, name, len(name))
		}
		manifest := r.cacheManifest(repo)
		if manifest.Name != name {
			t.Errorf("the manifest of %s is named %q, want %q", repo, manifest.Name, name)
		}
		secret := r.cacheSecretMeta(&manifest, repo)
		for what, meta := range map[string]struct{ labels, annotations map[string]string }{
			"Sandbox":      {manifest.Labels, manifest.Annotations},
			"pod template": {manifest.Spec.PodTemplate.Metadata.Labels, manifest.Spec.PodTemplate.Metadata.Annotations},
			"Secret":       {secret.Labels, secret.Annotations},
		} {
			if meta.labels[labelCache] != cacheMarker || meta.labels[labelProject] != testProject {
				t.Errorf("the %s of %s's cache is labelled %v, want the project and the cache marker", what, repo, meta.labels)
			}
			for _, key := range []string{labelIssue, labelTree, labelRole, labelProbe} {
				if _, ok := meta.labels[key]; ok {
					t.Errorf("the %s of %s's cache carries %s", what, repo, key)
				}
			}
			for key, value := range meta.labels {
				if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
					t.Errorf("the %s of %s's cache has label %s=%q, not a label value: %v", what, repo, key, value, errs)
				}
			}
			if got := meta.annotations[annotationCacheRepo]; got != repo.String() {
				t.Errorf("the %s of %s's cache is annotated %q, want %q", what, repo, got, repo.String())
			}
			if got, err := cacheRepository(meta.annotations); err != nil || got != repo {
				t.Errorf("cacheRepository of the %s of %s's cache = %s, %v", what, repo, got, err)
			}
		}
	}
	if a, b := CacheSandboxName(testProject, ghrepo.MustParse("acme/widgets-x")), CacheSandboxName(testProject, ghrepo.MustParse("acme-widgets/x")); a == b {
		t.Errorf("acme/widgets-x and acme-widgets/x share the cache name %s", a)
	}
	for _, annotations := range []map[string]string{nil, {annotationCacheRepo: "acme"}} {
		if _, err := cacheRepository(annotations); err == nil || !strings.Contains(err.Error(), annotationCacheRepo) {
			t.Errorf("cacheRepository(%v) = %v, want an error naming %s", annotations, err, annotationCacheRepo)
		}
	}
}

// A repository's cache is made as an issue pod is: its Sandbox created Suspended, with a headless
// Service, then its Secret written owned by the created Sandbox's uid, holding the implement App's
// token for the repository and a random bearer token of the cache's own, then one patch
// sets its pod template and Running. The pod runs the cache process alone, ready once /readyz
// answers, with both tokens read-only and the volume at CacheRoot, placed as every Legion pod is,
// and with nothing of an agent's: no providers, no Dispatch. A cache already as it should be is
// left alone, its bearer token unchanged.
func TestEnsureRepositoryCacheCreatesSuspendedWritesTheSecretThenSetsRunning(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	name := CacheSandboxName(testProject, cacheRepo)
	g.clearActions()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatalf("EnsureRepositoryCache: %v", err)
	}
	var writes []action
	var order []string
	for _, w := range g.writes() {
		if w.resource == "sandboxes" || w.resource == "secrets" {
			writes = append(writes, w)
			order = append(order, w.String())
		}
	}
	if want := []string{"create sandboxes " + name, "create secrets " + cacheSecretName(name), "patch sandboxes " + name}; !slices.Equal(order, want) {
		t.Fatalf("EnsureRepositoryCache wrote %q, want %q", order, want)
	}
	created, err := decodeSandbox(writes[0].object.(*unstructured.Unstructured))
	if err != nil {
		t.Fatal(err)
	}
	if created.Spec.OperatingMode != modeSuspended || created.Spec.Service == nil || !*created.Spec.Service {
		t.Errorf("the cache Sandbox was created %q with service %v, want Suspended with a Service", created.Spec.OperatingMode, created.Spec.Service)
	}
	stored := g.sandbox(name)
	secret := writes[1].object.(*corev1.Secret)
	if refs := secret.OwnerReferences; len(refs) != 1 || refs[0].Kind != "Sandbox" || refs[0].Name != name || refs[0].UID != stored.UID {
		t.Errorf("the cache Secret is owned by %+v, want the Sandbox %s uid %s", refs, name, stored.UID)
	}
	bearer := string(secret.Data[cacheTokenKey])
	if got := string(secret.Data[provisionTokenKey]); got != "ghs_provision_acme" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(bearer) || len(secret.Data) != 2 {
		t.Errorf("the cache Secret holds %v, want the implement token for acme and a 32-byte bearer token", secret.Data)
	}
	template := templatePatch(t, writes[2].patch)
	if template == nil || !modePatched(t, writes[2].patch, modeRunning) {
		t.Fatalf("the last write %q sets no pod template and Running", writes[2].patch)
	}
	spec := template.Spec
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
		t.Fatalf("the cache pod runs %d containers and %d init containers, want the cache alone", len(spec.Containers), len(spec.InitContainers))
	}
	c := spec.Containers[0]
	if want := []string{"/opt/legion/bin/legion", "workspace-init", "cache", "--repo", "acme/widgets", "--root", CacheRoot, "--listen", ":8418", "--refresh", "10m0s"}; c.Name != cacheContainer || !slices.Equal(c.Command, want) || c.Image != testImage {
		t.Errorf("the cache container is %s running %q on %s, want %s running %q on %s", c.Name, c.Command, c.Image, cacheContainer, want, testImage)
	}
	if p := c.ReadinessProbe; p == nil || p.HTTPGet == nil || p.HTTPGet.Path != "/readyz" || p.HTTPGet.Port.IntValue() != 8418 || len(p.HTTPGet.HTTPHeaders) != 0 {
		t.Errorf("the cache container's readiness probe is %+v, want GET /readyz on 8418 with no header", p)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if m := mounts[ProvisionDir]; m.Name != provisionVolume || !m.ReadOnly {
		t.Errorf("the token is mounted %+v, want %s read-only at %s", m, provisionVolume, ProvisionDir)
	}
	if m := mounts[CacheRoot]; m.Name != cacheVolume || m.ReadOnly {
		t.Errorf("the cache volume is mounted %+v, want %s at %s", m, cacheVolume, CacheRoot)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["LEGION_PROVISION_TOKEN_FILE"] != ProvisionDir+"/"+provisionTokenKey || env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] != ProvisionDir+"/"+cacheTokenKey {
		t.Errorf("LEGION_PROVISION_TOKEN_FILE = %q, LEGION_REPOSITORY_CACHE_TOKEN_FILE = %q", env["LEGION_PROVISION_TOKEN_FILE"], env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"])
	}
	for name, value := range env {
		if strings.HasPrefix(name, "DISPATCH") || strings.HasSuffix(name, "_FILE") && name != "LEGION_PROVISION_TOKEN_FILE" && name != "LEGION_REPOSITORY_CACHE_TOKEN_FILE" || value == bearer {
			t.Errorf("the cache container is told %s=%q", name, value)
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == providersVolume {
			t.Errorf("the cache pod mounts the providers Secret")
		}
		if v.Name == provisionVolume && (v.Secret == nil || v.Secret.SecretName != cacheSecretName(name) || len(v.Secret.Items) != 2) {
			t.Errorf("the token volume is %+v, want both keys of the cache Secret %s", v, cacheSecretName(name))
		}
		if v.Name == cacheVolume && (v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != cacheVolume+"-"+name) {
			t.Errorf("the cache volume is %+v, want the claim %s", v, cacheVolume+"-"+name)
		}
	}
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != gvisor || spec.NodeSelector[poolKey] != poolValue || len(spec.Tolerations) == 0 || spec.Tolerations[0].Key != poolKey {
		t.Errorf("the cache pod is placed %v %v %v, want gVisor on the Legion pool", spec.RuntimeClassName, spec.NodeSelector, spec.Tolerations)
	}
	if spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("the cache pod restarts %s, want Always", spec.RestartPolicy)
	}
	if sc := spec.SecurityContext; sc == nil || sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("the cache pod's security context is %+v, want fsGroupChangePolicy OnRootMismatch, so a restart does not re-own the clone", sc)
	}
	if templates := stored.Spec.VolumeClaimTemplates; len(templates) != 1 || templates[0].Metadata.Name != cacheVolume || templates[0].Spec.Resources.Requests.Storage().String() != "20Gi" {
		t.Errorf("the cache Sandbox owns %+v, want one %s claim of 20Gi", templates, cacheVolume)
	}

	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	g.clearActions()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatalf("EnsureRepositoryCache again: %v", err)
	}
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("ensuring a cache as it should be wrote %q", writes)
	}
	if again := string(g.secret(cacheSecretName(name)).Data[cacheTokenKey]); again != bearer {
		t.Errorf("the bearer token moved from %s to %s", bearer, again)
	}
}

// The cache pod takes its tokens from files the kubelet keeps current (INFRA-1254).
// refreshCacheTokens rewrites the cache's Secret every minute, and the kubelet rewrites the files
// of a Secret volume in place when that Secret changes — but only for a volume mounted whole: a
// mount with a non-empty subPath is copied once when the container starts and never written
// again, and a value taken through an environment secretKeyRef or an envFrom is fixed for the
// container's life. So the refresher's write reaches the running process only while three things
// hold, which this test pins: the cache container's mount of the provisioning volume carries an
// empty SubPath, that volume is the whole cache Secret with its two items (the provisioning token
// and the bearer token its clients present), and no container is handed either token in its
// environment — each is read from its projected file (LEGION_PROVISION_TOKEN_FILE,
// LEGION_REPOSITORY_CACHE_TOKEN_FILE). Undo any one of them and the refresh becomes nothing: the
// pod keeps serving the token it started with until it is replaced.
func TestTheCachePodProjectsItsTokenSoTheKubeletKeepsItCurrent(t *testing.T) {
	t.Parallel()
	name := CacheSandboxName(testProject, cacheRepo)
	spec := bareRuntime(t).cacheManifest(cacheRepo).Spec.PodTemplate.Spec
	cache := containerNamed(t, spec, cacheContainer)

	var mounts []corev1.VolumeMount
	for _, m := range cache.VolumeMounts {
		if m.Name == provisionVolume {
			mounts = append(mounts, m)
		}
	}
	if len(mounts) != 1 {
		t.Fatalf("%s mounts the %s volume %d times (%+v), want once", cacheContainer, provisionVolume, len(mounts), mounts)
	}
	if m := mounts[0]; m.SubPath != "" || m.MountPath != ProvisionDir {
		t.Errorf("%s mounts %s at %q with subPath %q, want the whole volume at %q with no subPath: the kubelet rewrites a whole-volume Secret projection in place when the Secret changes and never a subPath copy, so the refresher's write would not reach the file",
			cacheContainer, provisionVolume, m.MountPath, m.SubPath, ProvisionDir)
	}

	volume := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == provisionVolume })
	if volume < 0 {
		t.Fatalf("the cache pod has no %s volume, only %+v", provisionVolume, spec.Volumes)
	}
	secret := spec.Volumes[volume].Secret
	if secret == nil || secret.SecretName != cacheSecretName(name) {
		t.Fatalf("the %s volume is %+v, want the cache Secret %s whole", provisionVolume, spec.Volumes[volume], cacheSecretName(name))
	}
	var items []string
	for _, item := range secret.Items {
		items = append(items, item.Key+" at "+item.Path)
	}
	want := []string{provisionTokenKey + " at " + provisionTokenKey, cacheTokenKey + " at " + cacheTokenKey}
	if !slices.Equal(items, want) {
		t.Errorf("the %s volume projects %q, want %q", provisionVolume, items, want)
	}

	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		for _, v := range c.Env {
			if v.ValueFrom == nil || v.ValueFrom.SecretKeyRef == nil {
				continue
			}
			ref := v.ValueFrom.SecretKeyRef
			if ref.Key == provisionTokenKey || ref.Key == cacheTokenKey || v.Name == provisionTokenKey || v.Name == cacheTokenKey {
				t.Errorf("%s is handed %s from the Secret %s key %s; want the token's file alone, since an environment value is fixed for the container's life and no refresh reaches it",
					c.Name, v.Name, ref.Name, ref.Key)
			}
		}
		for _, from := range c.EnvFrom {
			if from.SecretRef != nil {
				t.Errorf("%s takes envFrom the Secret %s, which carries %s and %s; want the tokens' files alone, since an environment value is fixed for the container's life",
					c.Name, from.SecretRef.Name, provisionTokenKey, cacheTokenKey)
			}
		}
	}

	env := envOf(cache)
	if file := ProvisionDir + "/" + provisionTokenKey; env["LEGION_PROVISION_TOKEN_FILE"] != file {
		t.Errorf("LEGION_PROVISION_TOKEN_FILE = %q, want the projected file %q", env["LEGION_PROVISION_TOKEN_FILE"], file)
	}
	if file := ProvisionDir + "/" + cacheTokenKey; env[repocache.CacheTokenFileEnv] != file {
		t.Errorf("%s = %q, want the projected file %q", repocache.CacheTokenFileEnv, env[repocache.CacheTokenFileEnv], file)
	}
}

// The name of one repository's cache held by another repository's cache is a refusal naming both,
// and nothing is written.
func TestEnsureRepositoryCacheRefusesAnotherRepositorysCacheAtTheName(t *testing.T) {
	t.Parallel()
	g := newRig(t, []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", modeRunning, "acme/gadgets")})
	g.clearActions()
	err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo)
	if err == nil || !strings.Contains(err.Error(), "acme/gadgets") || !strings.Contains(err.Error(), "acme/widgets") {
		t.Fatalf("EnsureRepositoryCache = %v, want a refusal naming both repositories", err)
	}
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the refusal wrote %q", writes)
	}
}

// A cache whose pod runs another image, or failed, or whose pod template reserves other than the
// configuration, or a cache Sandbox found Suspended with no pod, is brought back as a relaunch
// brings an issue pod: suspended (when it is not), its old pod waited out, then its pod template
// set on the runtime's image and reservation and Running in one patch. The Sandbox and its volume
// are kept: nothing is deleted, its claim template is as it was, and its Secret is updated in
// place, owned by the same uid.
func TestEnsureRepositoryCacheReplacesADriftedPodKeepingTheSandbox(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mode    string
		pod     *corev1.Pod
		resized bool
		suspend bool
	}{
		{"a pod on another image", modeRunning, cachePod(cacheRepo, "uid-pod-old", "uid-cache", "ghcr.io/cybermonklabs/legion-worker@sha256:0ld", corev1.PodRunning), false, true},
		{"a failed pod", modeRunning, cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodFailed), false, true},
		{"a template reserving another cpu", modeRunning, cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodRunning), true, true},
		{"a template reserving another cpu, with no pod", modeRunning, nil, true, true},
		{"a Suspended Sandbox with no pod", modeSuspended, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := CacheSandboxName(testProject, cacheRepo)
			stale := &corev1.Secret{
				ObjectMeta: bareRuntime(t).cacheSecretMeta(&sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "uid-cache"}}, cacheRepo),
				Data:       map[string][]byte{provisionTokenKey: []byte("ghs_expired")},
			}
			cache := cacheObject(t, cacheRepo, "uid-cache", tc.mode, "")
			if tc.resized {
				cache = reserveCacheCPU(t, cache, "7")
			}
			objects := []k8sruntime.Object{cache, stale}
			if tc.pod != nil {
				objects = append(objects, tc.pod)
			}
			g := newRig(t, objects)
			before := g.sandbox(name).Spec.VolumeClaimTemplates
			g.clearActions()
			if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
				t.Fatalf("EnsureRepositoryCache: %v", err)
			}
			var suspended, running bool
			for _, w := range g.writes() {
				switch {
				case w.verb == "delete":
					t.Errorf("the replacement deleted %s", w)
				case w.resource == "secrets" && w.verb != "update":
					t.Errorf("the Secret was written by %s, want an update in place", w)
				case w.resource == "sandboxes" && w.verb == "patch" && modePatched(t, w.patch, modeSuspended):
					suspended = true
					if running {
						t.Error("the Sandbox was suspended after it was set Running")
					}
				case w.resource == "sandboxes" && w.verb == "patch" && modePatched(t, w.patch, modeRunning):
					running = true
					template := templatePatch(t, w.patch)
					if template == nil || template.Spec.Containers[0].Image != testImage {
						t.Errorf("the Running patch sets template %+v, want the runtime's image", template)
					} else if got := template.Spec.Containers[0].Resources; !equality.Semantic.DeepEqual(got, g.r.cacheConfig.Resources) {
						t.Errorf("the Running patch reserves %+v, want the configuration's %+v", got, g.r.cacheConfig.Resources)
					}
				}
			}
			if suspended != tc.suspend || !running {
				t.Errorf("suspended %t and set Running %t, want %t and true", suspended, running, tc.suspend)
			}
			if secret := g.secret(cacheSecretName(name)); secret == nil || string(secret.Data[provisionTokenKey]) != "ghs_provision_acme" || secret.OwnerReferences[0].UID != "uid-cache" {
				t.Errorf("the cache Secret is %+v, want the fresh token owned by uid-cache", secret)
			}
			after := g.sandbox(name)
			if after.UID != "uid-cache" || !jsonEqual(t, after.Spec.VolumeClaimTemplates, before) {
				t.Errorf("the cache Sandbox is %s with templates %+v, want uid-cache with %+v", after.UID, after.Spec.VolumeClaimTemplates, before)
			}
		})
	}
}

// reserveCacheCPU is the cache Sandbox u with its pod template's cache container reserving cpu,
// request and limit: a template written under another runtime.kubernetes.repository_cache.
func reserveCacheCPU(t *testing.T, u *unstructured.Unstructured, cpu string) *unstructured.Unstructured {
	t.Helper()
	s, err := decodeSandbox(u)
	if err != nil {
		t.Fatal(err)
	}
	c := &s.Spec.PodTemplate.Spec.Containers[0]
	c.Resources = *c.Resources.DeepCopy()
	c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cpu)
	c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpu)
	resized, err := encodeSandbox(*s)
	if err != nil {
		t.Fatal(err)
	}
	return resized
}

// A cache whose template reserves what the configuration does is left alone, though its pod's own
// requirements differ from the template's, as GKE Autopilot makes them at admission.
func TestEnsureRepositoryCacheReadsTheTemplateNotThePodForItsReservation(t *testing.T) {
	t.Parallel()
	adjusted := cachePod(cacheRepo, "uid-pod", "uid-cache", testImage, corev1.PodRunning)
	adjusted.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}
	g := newRig(t, []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", modeRunning, ""), adjusted})
	g.clearActions()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatalf("EnsureRepositoryCache: %v", err)
	}
	for _, w := range g.writes() {
		if w.resource == "sandboxes" || w.resource == "pods" {
			t.Errorf("a cache whose template reserves the configuration's resources was written: %s", w)
		}
	}
}

// A live cache pod due for replacement is left while its /healthz counts a stream it is sending or
// queueing, since the replacement would end every one of them, and replaced at the first ensure
// that finds it serving none.
func TestEnsureRepositoryCacheWaitsForTheStreamsToEndBeforeReplacingAPod(t *testing.T) {
	t.Parallel()
	var streams atomic.Int64
	streams.Store(2)
	served := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(workspace.CacheRepositoryHeader, cacheRepo.String())
		n := int(streams.Load())
		_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: cacheRepo.String(), Streams: &n})
	}))
	t.Cleanup(served.Close)
	live := cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodRunning)
	live.Status.PodIP = "10.0.0.5"
	g := newRig(t, []k8sruntime.Object{reserveCacheCPU(t, cacheObject(t, cacheRepo, "uid-cache", modeRunning, ""), "7"), live})
	g.r.cacheDial = (&podNetwork{routes: map[string]string{"10.0.0.5:8418": served.Listener.Addr().String()}}).dial
	g.clearActions()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatalf("EnsureRepositoryCache: %v", err)
	}
	for _, w := range g.writes() {
		if w.resource == "sandboxes" || w.resource == "pods" {
			t.Errorf("a cache pod sending 2 streams was replaced: %s", w)
		}
	}
	streams.Store(0)
	g.clearActions()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatalf("EnsureRepositoryCache: %v", err)
	}
	running := false
	for _, w := range g.writes() {
		if w.resource == "sandboxes" && w.verb == "patch" && modePatched(t, w.patch, modeRunning) {
			running = true
		}
	}
	if !running {
		t.Errorf("a cache pod serving no stream was not replaced: writes %v", g.writes())
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}

// The token refresh brings a cache's Secret back when it is lost, owned by the Sandbox's uid with a
// new bearer token, and replaces one another uid owns, keeping the bearer token it held, without
// touching the Sandbox; a Secret holding the fresh lease already is not written.
func TestRefreshCacheTokensRewritesALostOrForeignSecret(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	s := g.sandbox(name)
	secretsGVR := corev1.SchemeGroupVersion.WithResource("secrets")

	if err := g.kube.Tracker().Delete(secretsGVR, testNamespace, cacheSecretName(name)); err != nil {
		t.Fatal(err)
	}
	g.r.refreshCacheTokens(g.ctx)
	if secret := g.secret(cacheSecretName(name)); secret == nil || secret.OwnerReferences[0].UID != s.UID || string(secret.Data[provisionTokenKey]) != "ghs_provision_acme" || len(secret.Data[cacheTokenKey]) != 64 {
		t.Fatalf("after a refresh the lost Secret is %+v, want it back owned by %s with a bearer token", secret, s.UID)
	}
	bearer := string(g.secret(cacheSecretName(name)).Data[cacheTokenKey])
	if after := g.sandbox(name); after.mode() != s.mode() || after.Generation != s.Generation {
		t.Errorf("the refresh moved the Sandbox from %s/%d to %s/%d", s.mode(), s.Generation, after.mode(), after.Generation)
	}

	foreign := g.secret(cacheSecretName(name))
	foreign.OwnerReferences[0].UID = "uid-other"
	foreign.Data[provisionTokenKey] = []byte("ghs_provision_acme")
	if err := g.kube.Tracker().Update(secretsGVR, foreign, testNamespace); err != nil {
		t.Fatal(err)
	}
	g.clearActions()
	g.r.refreshCacheTokens(g.ctx)
	if writes := g.sandboxWrites(); !slices.Equal(writes, []string{"delete secrets " + cacheSecretName(name), "create secrets " + cacheSecretName(name)}) {
		t.Errorf("the refresh of a foreign Secret wrote %q, want it replaced", writes)
	}
	if secret := g.secret(cacheSecretName(name)); secret.OwnerReferences[0].UID != s.UID || string(secret.Data[cacheTokenKey]) != bearer {
		t.Errorf("the replaced Secret is owned by %s holding %q, want %s holding %q", secret.OwnerReferences[0].UID, secret.Data[cacheTokenKey], s.UID, bearer)
	}

	g.clearActions()
	g.r.refreshCacheTokens(g.ctx)
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the refresh of a current Secret wrote %q", writes)
	}
}

// A cache is ready, at its Service's http://<sandbox>.<namespace>.svc:8418, when the pod its Sandbox
// owns has passed its readiness probe, and not before, whatever address the pod holds; a long
// repository is found by the name it derives, with its whole name; and a cache name another
// repository's cache holds answers not found, logged once.
func TestRepositoryCacheReadsReadiness(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	g := newRig(t, []k8sruntime.Object{cacheObject(t, ghrepo.MustParse("acme/gadgets"), "uid-gadgets", modeRunning, "acme/sprockets")}, withLog(logs))
	for _, repo := range []ghrepo.Repository{cacheRepo, longRepo} {
		if err := g.r.EnsureRepositoryCache(g.ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	status, found := g.r.RepositoryCache(cacheRepo)
	if !found || status.Ready || status.Address != "" || status.Sandbox != name || status.Repo != cacheRepo {
		t.Fatalf("RepositoryCache before readiness = %+v, %t; want found, not ready, no address", status, found)
	}
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status.PodIP = "10.0.0.5"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	g.eventually("the cache ready", func() bool { status, _ := g.r.RepositoryCache(cacheRepo); return status.Ready })
	want := "http://" + name + "." + testNamespace + ".svc:8418"
	if status, _ := g.r.RepositoryCache(cacheRepo); status.Address != want || !status.RetainedUntil.IsZero() {
		t.Errorf("RepositoryCache once ready = %+v, want %s and no retention", status, want)
	}
	if status, found := g.r.RepositoryCache(longRepo); !found || status.Repo != longRepo {
		t.Errorf("RepositoryCache(%s) = %+v, %t; want found with the whole repository", longRepo, status, found)
	}
	for range 2 {
		if status, found := g.r.RepositoryCache(ghrepo.MustParse("acme/gadgets")); found {
			t.Errorf("the name acme/sprockets's cache holds answered %+v for acme/gadgets", status)
		}
	}
	if n := strings.Count(logs.String(), "acme/sprockets"); n != 1 {
		t.Errorf("the occupied name was logged %d times, want once:\n%s", n, logs)
	}
}

// crashLooping is pod with its cache container waiting to restart after failing four times.
func crashLooping(pod *corev1.Pod) *corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: cacheContainer, RestartCount: 4,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}},
	}}
	return pod
}

// A cache's state, as legion state shows it and a failed fetch names it: starting while its pod
// is absent or not yet running, cloning while its container runs and it is not ready, failing, with
// the kubelet's reason, while its container waits to restart or its image cannot be pulled, still
// starting while its readiness probe passes before the controller has made its Service, and ready,
// at that Service, once both are true.
func TestRepositoryCacheReadsItsState(t *testing.T) {
	t.Parallel()
	g := newRig(t, []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", modeRunning, "")}, withoutController())
	name := CacheSandboxName(testProject, cacheRepo)
	state := func() (string, string) {
		status, _ := g.r.RepositoryCache(cacheRepo)
		return status.State, status.Reason
	}
	if got, _ := state(); got != cacheStarting {
		t.Fatalf("a cache with no pod is %q, want %q", got, cacheStarting)
	}
	pod := cachePod(cacheRepo, "uid-cache-pod", "uid-cache", testImage, corev1.PodPending)
	if err := g.kube.Tracker().Add(pod); err != nil {
		t.Fatal(err)
	}
	g.eventually("the pending pod", func() bool { return g.r.storedPod(name) != nil })
	if got, _ := state(); got != cacheStarting {
		t.Errorf("a cache whose pod is pending is %q, want %q", got, cacheStarting)
	}
	for _, tc := range []struct {
		edit   func(*corev1.Pod)
		state  string
		reason string
	}{
		{func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodRunning
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: cacheContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}, cacheCloning, ""},
		{func(p *corev1.Pod) { crashLooping(p) }, cacheFailing, "container cache CrashLoopBackOff: back-off 5m0s restarting failed container (restarted 4 times)"},
		{func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: cacheContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"}}}}
		}, cacheFailing, "container cache ImagePullBackOff: pull access denied (restarted 0 times)"},
		{func(p *corev1.Pod) {
			p.Status.PodIP = "10.0.0.5"
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: cacheContainer, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}, cacheStarting, ""},
	} {
		g.update(g.pod(name), tc.edit)
		// Two steps in a row are failing: the step is the store's once its reason is the step's too.
		g.eventually(fmt.Sprintf("the cache %s, saying %q", tc.state, tc.reason), func() bool {
			got, reason := state()
			return got == tc.state && reason == tc.reason
		})
	}
	if err := g.kube.Tracker().Add(cacheService(name, "uid-cache")); err != nil {
		t.Fatal(err)
	}
	g.eventually("the cache ready behind its Service", func() bool {
		status, _ := g.r.RepositoryCache(cacheRepo)
		return status.State == cacheReady && status.Ready && status.Address == g.r.cacheURL(name)
	})
}

// The boot census keeps a cache Sandbox the runtime builds and refuses, by name, a Sandbox marked
// as a cache that is not one: no repository annotation, an issue's claim template, or a container
// more. An issue Sandbox owning a cache's claim template is still refused as an issue Sandbox.
func TestCensusAcceptsACacheSandboxAndRefusesAMalformedOne(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	g.spawn(rootSpec(t))
	sandboxes := g.dyn.Resource(sandboxGVR).Namespace(testNamespace)
	if err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject, g.opts.IssueDisks(), slogTo(io.Discard)); err != nil {
		t.Fatalf("census of a cache Sandbox and an issue pod = %v, want none", err)
	}
	malformed := map[string]func(u *unstructured.Unstructured){
		"no-annotation": func(u *unstructured.Unstructured) {
			unstructured.RemoveNestedField(u.Object, "metadata", "annotations")
		},
		"issue-template": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"metadata": map[string]any{"name": issueVolume}}}, "spec", "volumeClaimTemplates")
		},
		"two-containers": func(u *unstructured.Unstructured) {
			containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "podTemplate", "spec", "containers")
			_ = unstructured.SetNestedSlice(u.Object, append(containers, map[string]any{"name": "sidecar"}), "spec", "podTemplate", "spec", "containers")
		},
	}
	for suffix, edit := range malformed {
		u := cacheObject(t, ghrepo.MustParse("acme/"+suffix), "uid-"+suffix, modeRunning, "")
		edit(u)
		if err := g.dyn.Tracker().Create(sandboxGVR, u, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	issue := sandboxObject(t, "legion-legion-legion-300", "uid-300", modeSuspended, map[string]string{labelProject: testProject, labelTree: "LEGION-300", labelIssue: "LEGION-300"})
	_ = unstructured.SetNestedSlice(issue.Object, []any{map[string]any{"metadata": map[string]any{"name": cacheVolume}}}, "spec", "volumeClaimTemplates")
	containers := []any{}
	for _, role := range []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"} {
		containers = append(containers, map[string]any{"name": role})
	}
	_ = unstructured.SetNestedSlice(issue.Object, containers, "spec", "podTemplate", "spec", "containers")
	if err := g.dyn.Tracker().Create(sandboxGVR, issue, testNamespace); err != nil {
		t.Fatal(err)
	}
	err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject, g.opts.IssueDisks(), slogTo(io.Discard))
	if err == nil {
		t.Fatal("census of malformed caches passed")
	}
	for suffix := range malformed {
		if !strings.Contains(err.Error(), "cache Sandbox "+CacheSandboxName(testProject, ghrepo.MustParse("acme/"+suffix))) {
			t.Errorf("census = %q, want it to refuse the cache Sandbox of acme/%s", err, suffix)
		}
	}
	if !strings.Contains(err.Error(), "Sandbox legion-legion-legion-300 owns a volume claim template named \"cache\"") {
		t.Errorf("census = %q, want the issue Sandbox with a cache template refused by its layout", err)
	}
	if strings.Contains(err.Error(), CacheSandboxName(testProject, cacheRepo)) {
		t.Errorf("census = %q, which refuses the cache this runtime built", err)
	}
}

// The orphan sweep makes the missing cache of a repository of the cache set, keeps it, replacing a
// pod of it that drifted, and retires one outside the set: marked with when it left the set, kept
// while that is under Retain, then deleted in the foreground with its volume; one back in the set
// before then loses the mark and stays. The claim path's sweep never judges a cache.
func TestReconcileOrphansKeepsACacheInTheSetAndRetiresOneOutsideIt(t *testing.T) {
	t.Parallel()
	var inSet atomic.Bool
	inSet.Store(true)
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) {
		o.CacheRepositories = func() []ghrepo.Repository {
			if inSet.Load() {
				return []ghrepo.Repository{cacheRepo}
			}
			return nil
		}
	}))
	var propagation []metav1.DeletionPropagation
	g.dyn.PrependReactor("delete", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		if policy := a.(k8stesting.DeleteActionImpl).DeleteOptions.PropagationPolicy; policy != nil {
			propagation = append(propagation, *policy)
		}
		return false, nil, nil
	})
	name := CacheSandboxName(testProject, cacheRepo)
	sweep := func(what string) {
		t.Helper()
		g.clearActions()
		if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
			t.Fatalf("ReconcileOrphans %s: %v", what, err)
		}
	}
	unusedAt := func() string {
		g.t.Helper()
		if s := g.sandbox(name); s != nil {
			return s.Annotations[annotationCacheUnusedAt]
		}
		return "gone"
	}

	sweep("with a repository of the set that has no cache")
	if writes := g.sandboxWrites(); !slices.Contains(writes, "create sandboxes "+name) {
		t.Fatalf("the sweep wrote %q, want the cache of the set's repository made", writes)
	}
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	g.eventually("the Running cache in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.mode() == modeRunning
	})

	sweep("of a cache in the set")
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the sweep of a cache in the set wrote %q", writes)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("the sweep of a cache in the set warned:\n%s", logs)
	}

	inSet.Store(false)
	sweep("of a cache outside the set")
	marked := unusedAt()
	if at, err := time.Parse(time.RFC3339, marked); err != nil || !at.Equal(rigNow.Truncate(time.Second)) {
		t.Fatalf("the cache outside the set is marked %q, want the sweep's time %s", marked, rigNow)
	}
	g.eventually("the mark in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Annotations[annotationCacheUnusedAt] == marked
	})
	g.advance(167 * time.Hour)
	sweep("within the retention")
	if unusedAt() != marked || len(propagation) != 0 {
		t.Fatalf("the cache inside its retention is %q, deleted %v; want it kept as marked", unusedAt(), propagation)
	}

	inSet.Store(true)
	sweep("of a cache back in the set")
	if got := unusedAt(); got != "" {
		t.Fatalf("the cache back in the set is still marked %q", got)
	}
	g.eventually("the patch in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Annotations[annotationCacheUnusedAt] == ""
	})

	g.update(g.pod(name), func(p *corev1.Pod) { p.Spec.Containers[0].Image = "ghcr.io/cybermonklabs/legion-worker@sha256:0ld" })
	g.eventually("the drifted pod in the store", func() bool {
		_, drifted := imageDrift(g.r.storedPod(name), testImage)
		return drifted
	})
	sweep("of a cache in the set whose pod drifted")
	var suspended, running bool
	for _, w := range g.writes() {
		if w.resource == "sandboxes" && w.verb == "patch" {
			suspended = suspended || modePatched(t, w.patch, modeSuspended)
			running = running || modePatched(t, w.patch, modeRunning)
		}
	}
	if !suspended || !running {
		t.Errorf("the sweep of a drifted cache suspended %t and set Running %t, want both", suspended, running)
	}

	inSet.Store(false)
	sweep("of a cache outside the set again")
	g.eventually("the mark in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Annotations[annotationCacheUnusedAt] != ""
	})
	g.advance(168 * time.Hour)
	sweep("past the retention")
	if g.sandbox(name) != nil || !slices.Equal(propagation, []metav1.DeletionPropagation{metav1.DeletePropagationForeground}) {
		t.Fatalf("the cache past its retention is %v, deleted with %v; want it deleted in the foreground", g.sandbox(name), propagation)
	}
	if !strings.Contains(logs.String(), "acme/widgets") || !strings.Contains(logs.String(), "168h0m0s") {
		t.Errorf("the retirement was not logged with its repository and how long it was kept:\n%s", logs)
	}
}

// The set the sweep judges caches against is the repositories of the project's live issue records
// and of the launches since the daemon last read them (Options.CacheRepositories), so a
// repository gets and keeps a cache for as long as a live record names it: one volume and one pod
// per such repository. Once no record names it — its last issue is done — the cache is marked,
// kept for its retention and retired, which is the one rule the sweep has, and the cache of a
// repository a live record still names is kept.
func TestReconcileOrphansKeepsTheCacheOfARepositoryWhileARecordNamesIt(t *testing.T) {
	t.Parallel()
	// A live record names acme/widgets throughout; stranger/tools is named by a live record until
	// its issue is done.
	recorded := ghrepo.MustParse("stranger/tools")
	var live atomic.Bool
	live.Store(true)
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) {
		o.CacheRepositories = func() []ghrepo.Repository {
			if live.Load() {
				return []ghrepo.Repository{cacheRepo, recorded}
			}
			return []ghrepo.Repository{cacheRepo}
		}
	}))
	name := CacheSandboxName(testProject, recorded)
	sweep := func(what string) {
		t.Helper()
		g.clearActions()
		if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
			t.Fatalf("ReconcileOrphans %s: %v", what, err)
		}
	}

	sweep("with a repository a record names")
	if writes := g.sandboxWrites(); !slices.Contains(writes, "create sandboxes "+name) {
		t.Fatalf("the sweep wrote %q, want the cache of the repository a record names made", writes)
	}
	g.eventually("the cache pod of "+recorded.String(), func() bool { return g.r.storedPod(name) != nil })

	sweep("of the cache of a repository a live record still names")
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the sweep of a cache a live record names wrote %q, want it kept untouched", writes)
	}
	if s := g.sandbox(name); s == nil || s.Annotations[annotationCacheUnusedAt] != "" {
		t.Fatalf("the cache of %s is %v, want it kept unmarked while a record names it", recorded, s)
	}

	live.Store(false)
	sweep("once no record names it")
	g.eventually("the mark in the store", func() bool {
		s, _ := g.r.storedSandbox(name)
		return s != nil && s.Annotations[annotationCacheUnusedAt] != ""
	})
	g.advance(168 * time.Hour)
	sweep("past the retention")
	if g.sandbox(name) != nil {
		t.Fatalf("the cache of %s past its retention is %v, want it retired", recorded, g.sandbox(name))
	}
	if g.sandbox(CacheSandboxName(testProject, cacheRepo)) == nil {
		t.Fatal("the cache of acme/widgets, which a live record still names, was retired with it, want it kept")
	}
	if !strings.Contains(logs.String(), recorded.String()) {
		t.Errorf("the retirement was not logged with its repository:\n%s", logs)
	}
}

// The sweep reads a cache's repository as its annotation spells it, against the cache set, and
// resolves no configured rename: nothing under this package does. With the pair's two names both
// named in the configuration and the set holding the pair's served name alone, the repository
// GitHub holds the pair under now, the cache of that served name is kept and the cache of the
// recorded name is marked, to be kept for its retention and retired after it, as any cache whose
// repository left the set is.
//
// That is the sweep's verdict today, and resolving a rename per repository rather than per owner
// does not move it: the sweep asks the set about a name, never about an owner, so neither
// state of the owner set changes what it keeps. While the recorded name's cache is kept it stays
// at its own address, with a token minted for the owner serving the repository now, which is what
// keeps the pods addressed at it fetching.
func TestTheSweepKeepsTheServedNamesCacheAndMarksTheRecordedOnes(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withOptions(servingRenamed(servingSmoke)))
	for _, repo := range []ghrepo.Repository{recordedSmoke, servingSmoke} {
		if err := g.r.EnsureRepositoryCache(g.ctx, repo); err != nil {
			t.Fatalf("make the cache of %s: %v", repo, err)
		}
		name := CacheSandboxName(testProject, repo)
		g.eventually("the cache pod of "+repo.String(), func() bool { return g.r.storedPod(name) != nil })
	}
	kept, unused := CacheSandboxName(testProject, servingSmoke), CacheSandboxName(testProject, recordedSmoke)

	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatalf("ReconcileOrphans over both of the pair's caches: %v", err)
	}

	g.eventually("the recorded name's cache marked", func() bool {
		s, _ := g.r.storedSandbox(unused)
		return s != nil && s.Annotations[annotationCacheUnusedAt] != ""
	})
	marked := g.sandbox(unused)
	if marked == nil || marked.Annotations[annotationCacheRepo] != recordedSmoke.String() {
		t.Fatalf("the cache of %s is %+v, want it kept at its own address, annotated with the repository the record names", recordedSmoke, marked)
	}
	if at, err := time.Parse(time.RFC3339, marked.Annotations[annotationCacheUnusedAt]); err != nil || !at.Equal(rigNow.Truncate(time.Second)) {
		t.Errorf("the cache of %s is marked %q, want the sweep's time %s: the set holds the pair's served name alone, %s", recordedSmoke, marked.Annotations[annotationCacheUnusedAt], rigNow, servingSmoke)
	}
	if s := g.sandbox(kept); s == nil || s.Annotations[annotationCacheUnusedAt] != "" {
		t.Errorf("the cache of %s is %+v, want the cache of the set's repository kept and marked with nothing", servingSmoke, s)
	}
	for name, repo := range map[string]ghrepo.Repository{kept: servingSmoke, unused: recordedSmoke} {
		if got := string(g.secret(cacheSecretName(name)).Data[provisionTokenKey]); got != "ghs_provision_"+servingSmoke.Owner() {
			t.Errorf("the cache of %s holds the token %q, want one minted for %s, the owner serving the repository now", repo, got, servingSmoke.Owner())
		}
	}
}

// A cache the sweep of a release before INFRA-4401 marked under the old annotation
// (annotationCacheOldUnusedAt) loses it at the first pass of this release's sweep, as INFRA-4401's
// A4 requires, and retires one retention after its first mark, the earlier of its marks that parse
// as a time. Five caches meet one first pass:
//  1. outside the cache set, marked under the old annotation half a retention before the sweep: it
//     carries that time under annotationCacheUnusedAt and no old annotation, is kept at a sweep one
//     second short of one retention from that mark, and is retired at the first sweep after, one
//     retention from it;
//  2. outside the set, its old mark older than one retention: it is retired at that pass;
//  3. in the set: it loses the old annotation and carries no new one;
//  4. outside the set, carrying both marks, the old one the earlier: it carries the old mark's time
//     under annotationCacheUnusedAt and no old annotation;
//  5. outside the set, its old mark not a time: it carries a mark of the sweep's time and no old
//     annotation, and one WARN line names the cache and the value.
//
// Each has one INFO line naming the cache, its repository, the old annotation's value and what
// the pass did. The old marks are written as the literal, as an earlier release left them;
// dispatch://INFRA-3532's pull request deletes the literal and this test with
// annotationCacheOldUnusedAt. The controls, each run in a copy of the tree: the old annotation
// left in place (markUnused's patch without annotationCacheOldUnusedAt), which fails cases 1, 3, 4
// and 5, each cache still carrying it; the old mark ignored (sweepOldMarkedCache never taking
// annotationCacheOldUnusedAt as a first mark), which fails case 1, marked at the sweep's time,
// case 4, its later mark kept, and case 2, kept and not retired; the later mark kept (the old mark
// read only where the new one is absent or not a time), which fails case 4; and a cache none of
// whose marks parses kept untouched, which fails case 5, still carrying the old annotation.
func TestACacheMarkedUnderTheOldAnnotationRetiresOneRetentionAfterItsFirstMark(t *testing.T) {
	t.Parallel()
	const oldAnnotation = "legion.dev/cache-unmapped-at"
	inSet := ghrepo.MustParse("acme/sprockets")
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) {
		o.CacheRepositories = func() []ghrepo.Repository { return []ghrepo.Repository{inSet} }
	}))
	retain := g.r.cacheConfig.Retain
	stamp := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	halfway, expired := rigNow.Add(-retain/2), rigNow.Add(-retain-retain/2)
	earlier, later := rigNow.Add(-24*time.Hour), rigNow.Add(-time.Hour)
	// caches are the five cases, in order: each one's repository, the marks an earlier release left
	// on its cache, and the words its INFO line holds besides the cache, repository and old value.
	caches := []struct {
		repo  ghrepo.Repository
		marks map[string]string
		did   []string
	}{
		{cacheRepo, map[string]string{oldAnnotation: stamp(halfway)},
			[]string{"repository cache marked at its first mark, its old annotation removed", annotationCacheUnusedAt + "=" + stamp(halfway)}},
		{ghrepo.MustParse("acme/gadgets"), map[string]string{oldAnnotation: stamp(expired)},
			[]string{"retired a repository cache no longer in the cache set", "kept=" + (retain + retain/2).String()}},
		{inSet, map[string]string{oldAnnotation: stamp(halfway)},
			[]string{"repository cache back in the cache set; its marks cleared"}},
		{ghrepo.MustParse("acme/gizmos"), map[string]string{oldAnnotation: stamp(earlier), annotationCacheUnusedAt: stamp(later)},
			[]string{"repository cache marked at its first mark, its old annotation removed", annotationCacheUnusedAt + "=" + stamp(earlier)}},
		{ghrepo.MustParse("acme/doohickeys"), map[string]string{oldAnnotation: "yesterday"},
			[]string{"repository cache no longer in the cache set; kept for its retention"}},
	}
	names := make([]string, len(caches))
	for i, c := range caches {
		name := CacheSandboxName(testProject, c.repo)
		names[i] = name
		if err := g.r.EnsureRepositoryCache(g.ctx, c.repo); err != nil {
			t.Fatalf("make the cache of %s: %v", c.repo, err)
		}
		g.eventually("the cache pod of "+c.repo.String(), func() bool { return g.r.storedPod(name) != nil })
		s := g.sandbox(name)
		if s == nil {
			t.Fatalf("the cache Sandbox %s is absent", name)
		}
		maps.Copy(s.Annotations, c.marks)
		u, err := encodeSandbox(*s)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
			t.Fatal(err)
		}
		g.eventually("the marks of "+c.repo.String()+" in the store", func() bool {
			stored, _ := g.r.storedSandbox(name)
			if stored == nil {
				return false
			}
			for key, value := range c.marks {
				if stored.Annotations[key] != value {
					return false
				}
			}
			return true
		})
	}
	sweep := func(what string) {
		t.Helper()
		if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
			t.Fatalf("ReconcileOrphans %s: %v", what, err)
		}
	}
	// marked answers case i+1's cache as the first pass left it, failing the test when it is gone
	// or still carries the old annotation.
	marked := func(i int) *sandbox {
		t.Helper()
		s := g.sandbox(names[i])
		if s == nil {
			t.Errorf("case %d: the first pass retired the cache of %s, want it kept", i+1, caches[i].repo)
			return nil
		}
		if old, held := s.Annotations[oldAnnotation]; held {
			t.Errorf("case %d: the cache of %s still carries %s=%q after the first pass, want it removed", i+1, caches[i].repo, oldAnnotation, old)
		}
		return s
	}

	before := logs.Len()
	sweep("the first pass, over five caches marked under the old annotation")

	if s := marked(0); s != nil {
		if got := s.Annotations[annotationCacheUnusedAt]; got != stamp(halfway) {
			t.Errorf("case 1: the cache is marked %q under %s, want its first mark's time %s: the sweep ignored the old mark", got, annotationCacheUnusedAt, stamp(halfway))
		}
	}
	if s := g.sandbox(names[1]); s != nil {
		t.Errorf("case 2: the cache whose old mark is older than one retention is %v after the first pass, want it retired at that pass: the sweep ignored the old mark", s.Annotations)
	}
	if s := marked(2); s != nil {
		if got, held := s.Annotations[annotationCacheUnusedAt]; held {
			t.Errorf("case 3: the cache in the set is marked %q under %s, want no mark", got, annotationCacheUnusedAt)
		}
	}
	if s := marked(3); s != nil {
		if got := s.Annotations[annotationCacheUnusedAt]; got != stamp(earlier) {
			t.Errorf("case 4: the cache is marked %q under %s, want the old mark's time %s, the earlier of its two: the sweep kept the later mark", got, annotationCacheUnusedAt, stamp(earlier))
		}
	}
	if s := marked(4); s != nil {
		if got := s.Annotations[annotationCacheUnusedAt]; got != stamp(rigNow) {
			t.Errorf("case 5: the cache whose old mark is not a time is marked %q under %s, want the sweep's time %s", got, annotationCacheUnusedAt, stamp(rigNow))
		}
	}
	logged := logs.String()[before:]
	for i, c := range caches {
		var lines []string
		for _, line := range linesHolding(logged, "sandbox="+names[i]+" ") {
			if strings.Contains(line, "level=INFO") && strings.Contains(line, oldAnnotation+"="+c.marks[oldAnnotation]) {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 {
			t.Errorf("case %d: the first pass logged %q for the cache of %s, want one INFO line naming its old annotation's value", i+1, lines, c.repo)
			continue
		}
		for _, want := range append([]string{"repo=" + c.repo.String()}, c.did...) {
			if !strings.Contains(lines[0], want) {
				t.Errorf("case %d: the INFO line of the cache of %s is %q, want it to hold %q", i+1, c.repo, lines[0], want)
			}
		}
	}
	if warned := linesHolding(logged, "level=WARN"); len(warned) != 1 || !strings.Contains(warned[0], "sandbox="+names[4]+" ") || !strings.Contains(warned[0], "repo="+caches[4].repo.String()) || !strings.Contains(warned[0], oldAnnotation+"=yesterday") {
		t.Errorf("the first pass warned %q, want one WARN line naming the cache of %s and its old mark, yesterday", warned, caches[4].repo)
	}

	g.eventually("case 1's first mark in the store", func() bool {
		stored, _ := g.r.storedSandbox(names[0])
		if stored == nil {
			return false
		}
		_, old := stored.Annotations[oldAnnotation]
		return stored.Annotations[annotationCacheUnusedAt] != "" && !old
	})
	g.advance(retain/2 - time.Second)
	sweep("one second short of one retention from case 1's first mark")
	if g.sandbox(names[0]) == nil {
		t.Fatal("case 1: the cache was retired before one retention had passed from its first mark")
	}
	g.advance(time.Second)
	sweep("one retention from case 1's first mark")
	if s := g.sandbox(names[0]); s != nil {
		t.Fatalf("case 1: the cache one retention from its first mark is %v, want it retired", s.Annotations)
	}
	if retired := linesHolding(logs.String(), "retired a repository cache no longer in the cache set"); !slices.ContainsFunc(retired, func(line string) bool {
		return strings.Contains(line, "sandbox="+names[0]+" ") && strings.Contains(line, "kept="+retain.String())
	}) {
		t.Errorf("case 1: the retirement was not logged with how long the cache was kept, one retention from its first mark: %q", retired)
	}
}

// A cache pod created under a ServiceAccount other than the configured one is made again, as one
// on another image is (accountDrift beside imageDrift): the account is fixed for the pod's life,
// so the cache's pod reaches the configured one no other way, and the pod the sweep makes names
// it. The Sandbox and the volume holding the clone are kept through the replacement: a drifted
// account costs a new pod, not a new clone. A cache pod already on the configured account is left
// alone, and one still sending or queueing a tar waits for its streams to end, whichever drift is
// replacing it.
func TestEnsureRepositoryCacheReplacesACachePodRunningAnotherServiceAccount(t *testing.T) {
	t.Parallel()
	name := CacheSandboxName(testProject, cacheRepo)
	// cacheOn is a rig configured to run its pods as otherAccount, holding a Running cache
	// Sandbox whose pod template that configuration wrote, so the template binds the account the
	// rig is configured with and only the pod can drift, and a live pod of it on podAccount, whose
	// /healthz counts serving streams when the case gives it any.
	cacheOn := func(t *testing.T, podAccount string, serving int) (*rig, *logBuffer) {
		t.Helper()
		live := cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodRunning)
		live.Spec.ServiceAccountName = podAccount
		var routes map[string]string
		if serving > 0 {
			served := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(workspace.CacheRepositoryHeader, cacheRepo.String())
				n := serving
				_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: cacheRepo.String(), Streams: &n})
			}))
			t.Cleanup(served.Close)
			live.Status.PodIP = "10.0.0.5"
			routes = map[string]string{"10.0.0.5:8418": served.Listener.Addr().String()}
		}
		logs := &logBuffer{}
		shared := func(o *Options) { o.Pod.ServiceAccount = otherAccount }
		written := cacheObjectOf(t, configuredRuntime(t, shared), cacheRepo, "uid-cache", modeRunning, "")
		g := newRig(t, []k8sruntime.Object{written, live}, withLog(logs), withOptions(shared))
		if routes != nil {
			g.r.cacheDial = (&podNetwork{routes: routes}).dial
		}
		g.clearActions()
		return g, logs
	}

	t.Run("a cache pod on another ServiceAccount is made again", func(t *testing.T) {
		g, _ := cacheOn(t, "", 0)
		before := g.sandbox(name)
		if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
			t.Fatalf("EnsureRepositoryCache: %v", err)
		}
		var suspended, running bool
		for _, w := range g.writes() {
			if w.resource == "sandboxes" && w.verb == "patch" {
				suspended = suspended || modePatched(t, w.patch, modeSuspended)
				running = running || modePatched(t, w.patch, modeRunning)
			}
		}
		if !suspended || !running {
			t.Errorf("the ensure of a cache pod on another account suspended %t and set Running %t, want both: writes %v", suspended, running, g.writes())
		}
		after := g.sandbox(name)
		if after == nil || after.UID != before.UID || !jsonEqual(t, after.Spec.VolumeClaimTemplates, before.Spec.VolumeClaimTemplates) {
			t.Fatalf("the cache Sandbox is %+v, want %s kept with its volume %+v", after, before.UID, before.Spec.VolumeClaimTemplates)
		}
		if got := after.Spec.PodTemplate.Spec.ServiceAccountName; got != otherAccount {
			t.Errorf("the pod template the replacement wrote runs as %q, want the configured %s", got, otherAccount)
		}
	})

	t.Run("a cache pod on the configured ServiceAccount is left alone", func(t *testing.T) {
		g, _ := cacheOn(t, otherAccount, 0)
		if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
			t.Fatalf("EnsureRepositoryCache: %v", err)
		}
		for _, w := range g.writes() {
			if w.resource == "sandboxes" || w.resource == "pods" {
				t.Errorf("a cache pod on the configured account was written: %s", w)
			}
		}
	})

	t.Run("a cache pod on another ServiceAccount serving a stream waits", func(t *testing.T) {
		g, logs := cacheOn(t, "", 2)
		if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
			t.Fatalf("EnsureRepositoryCache: %v", err)
		}
		for _, w := range g.writes() {
			if w.resource == "sandboxes" || w.resource == "pods" {
				t.Errorf("a cache pod on another account sending 2 streams was replaced: %s", w)
			}
		}
		if !strings.Contains(logs.String(), "repository cache pod is replaced once it serves no stream") {
			t.Errorf("the waiting replacement was not logged:\n%s", logs)
		}
	})
}

// testBucket and cacheAccount are the Cloud Storage bucket a cache keeps its packs in and the
// ServiceAccount the cache pods alone run as (runtime.kubernetes.repository_cache.bucket and
// service_account): an example bucket, and an account that is neither otherAccount nor the
// namespace's default.
const (
	testBucket   = "example-legion-packs"
	cacheAccount = "legion-cache"
)

// sharedAccount configures every pod to run as otherAccount, with the cache's bucket off.
func sharedAccount(o *Options) { o.Pod.ServiceAccount = otherAccount }

// withBucket is sharedAccount with the cache's bucket on: the cache pods alone run as account and
// are told bucket.
func withBucket(bucket, account string) func(*Options) {
	return func(o *Options) {
		sharedAccount(o)
		o.RepositoryCache.Bucket, o.RepositoryCache.ServiceAccount = bucket, account
	}
}

// cacheBinding is the bucket a cache pod's spec tells its cache container (repocache.BucketEnv),
// "" where it tells none, and the ServiceAccount the pod runs as, `default` where it names none.
func cacheBinding(t *testing.T, spec corev1.PodSpec) (bucket, account string) {
	t.Helper()
	return envOf(containerNamed(t, spec, cacheContainer))[repocache.BucketEnv], serviceAccountOr(spec.ServiceAccountName)
}

// The cache's bucket binds the cache pod alone, and the daemon keeps it bound as configured. A
// cache made under the bucket runs as the cache's own account and tells its container the bucket;
// one made without it runs as the account every pod runs as and tells none; and the two pods differ
// in nothing else: the argv, the Secret's projection and its mount, and the token automount, which
// stays off, since a pod's Google identity comes from GKE's metadata server rather than a mounted
// token. A Sandbox whose pod template, or whose live pod's account, binds the cache otherwise than
// the configuration now does is given a new pod: one on the shared account or an unrelated one
// while the bucket is on, a template written before the bucket was turned on, one written for
// another bucket or another cache account, and one written for the bucket after both keys were
// removed. The replacement keeps the Sandbox, by uid, and its volume claim template, and waits
// while the pod sends a tar. The next ensure finds the replacement as the configuration wants it
// and writes nothing. A pod on the cache's own account under the bucket, or on the shared account
// without it, is left alone. The row of a pod on the shared account under the bucket is the
// control: a drift check that took the shared account for the cache's own would leave that pod,
// with every pod's identity, holding the bucket's grant, and the row fails.
func TestRepositoryCacheBucketAccountReconciliation(t *testing.T) {
	t.Parallel()
	name := CacheSandboxName(testProject, cacheRepo)
	off, on := sharedAccount, withBucket(testBucket, cacheAccount)
	ensure := func(t *testing.T, g *rig) {
		t.Helper()
		if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
			t.Fatalf("EnsureRepositoryCache: %v", err)
		}
	}
	// podWrites are the ensure's writes to the cache's Sandbox and pods.
	podWrites := func(g *rig) []string {
		var out []string
		for _, w := range g.writes() {
			if w.resource == "sandboxes" || w.resource == "pods" {
				out = append(out, w.String())
			}
		}
		return out
	}
	// replaced is whether the ensure suspended the cache's Sandbox and then set it Running with a
	// pod template, which is how a cache pod is made again.
	replaced := func(t *testing.T, g *rig) bool {
		t.Helper()
		var suspended, running bool
		for _, w := range g.writes() {
			if w.resource == "sandboxes" && w.verb == "patch" {
				suspended = suspended || modePatched(t, w.patch, modeSuspended)
				running = running || suspended && modePatched(t, w.patch, modeRunning) && templatePatch(t, w.patch) != nil
			}
		}
		return suspended && running
	}

	t.Run("the bucket changes the cache pod's account and its one variable, and nothing else", func(t *testing.T) {
		without := configuredRuntime(t, off).cachePodTemplate(name, cacheRepo)
		with := configuredRuntime(t, on).cachePodTemplate(name, cacheRepo)
		if bucket, account := cacheBinding(t, without.Spec); bucket != "" || account != otherAccount {
			t.Errorf("without the bucket the cache pod is told bucket %q and runs as %s, want none and the shared %s", bucket, account, otherAccount)
		}
		if bucket, account := cacheBinding(t, with.Spec); bucket != testBucket || account != cacheAccount {
			t.Errorf("with the bucket the cache pod is told bucket %q and runs as %s, want %s and its own %s", bucket, account, testBucket, cacheAccount)
		}
		if automount := with.Spec.AutomountServiceAccountToken; automount == nil || *automount {
			t.Errorf("with the bucket the cache pod's token automount is %v, want false", automount)
		}
		unbound := *with.Spec.DeepCopy()
		unbound.ServiceAccountName = without.Spec.ServiceAccountName
		cache := &unbound.Containers[slices.IndexFunc(unbound.Containers, func(c corev1.Container) bool { return c.Name == cacheContainer })]
		cache.Env = slices.DeleteFunc(cache.Env, func(e corev1.EnvVar) bool { return e.Name == repocache.BucketEnv })
		if !equality.Semantic.DeepEqual(unbound, without.Spec) || !equality.Semantic.DeepEqual(with.Metadata, without.Metadata) {
			t.Errorf("past its account and %s the cache pod with the bucket is\n%+v\nwant the one without it\n%+v", repocache.BucketEnv, unbound, without.Spec)
		}
	})

	t.Run("a new cache is bound as configured and its pod is accepted", func(t *testing.T) {
		for _, tc := range []struct {
			name            string
			configured      func(*Options)
			bucket, account string
		}{
			{"bucket off", off, "", otherAccount},
			{"bucket on", on, testBucket, cacheAccount},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := newRig(t, nil, withOptions(tc.configured))
				ensure(t, g)
				if bucket, account := cacheBinding(t, g.sandbox(name).Spec.PodTemplate.Spec); bucket != tc.bucket || account != tc.account {
					t.Errorf("the new cache's pod template is told bucket %q and runs as %s, want %q and %s", bucket, account, tc.bucket, tc.account)
				}
				g.eventually("the cache's pod", func() bool { return g.r.storedPod(name) != nil })
				if account := serviceAccountOr(g.r.storedPod(name).Spec.ServiceAccountName); account != tc.account {
					t.Errorf("the cache's pod runs as %s, want %s", account, tc.account)
				}
				g.clearActions()
				ensure(t, g)
				if writes := podWrites(g); len(writes) > 0 {
					t.Errorf("ensuring a cache whose pod is bound as configured wrote %q", writes)
				}
			})
		}
	})

	for _, tc := range []struct {
		name string
		// written is the configuration the Sandbox's pod template was written under, and runs
		// the account its live pod runs as: "" for a Sandbox whose pod is not made yet, whose rig
		// runs no controller, so none is.
		written func(*Options)
		runs    string
		// configured is the configuration the ensure runs under, and replaced whether it makes
		// the pod again, told bucket and running as account.
		configured      func(*Options)
		replaced        bool
		bucket, account string
	}{
		{name: "the bucket off and a pod on the shared account are left alone", written: off, runs: otherAccount, configured: off},
		{name: "the bucket on and a pod on the cache's own account are left alone", written: on, runs: cacheAccount, configured: on},
		{name: "the bucket on and a pod on the shared account is made again", written: on, runs: otherAccount, configured: on,
			replaced: true, bucket: testBucket, account: cacheAccount},
		{name: "the bucket on and a pod on an unrelated account is made again", written: on, runs: "legion-stranger", configured: on,
			replaced: true, bucket: testBucket, account: cacheAccount},
		{name: "turning the bucket on makes the pod on the shared account again", written: off, runs: otherAccount, configured: on,
			replaced: true, bucket: testBucket, account: cacheAccount},
		{name: "turning the bucket on before the pod is made rewrites its template", written: off, configured: on,
			replaced: true, bucket: testBucket, account: cacheAccount},
		{name: "a change of the bucket alone makes the pod again", written: withBucket("example-legion-packs-old", cacheAccount), runs: cacheAccount, configured: on,
			replaced: true, bucket: testBucket, account: cacheAccount},
		{name: "a change of the cache's account makes the pod again", written: withBucket(testBucket, "legion-cache-old"), runs: "legion-cache-old", configured: on,
			replaced: true, bucket: testBucket, account: cacheAccount},
		{name: "removing both keys makes the pod on the cache's account again", written: on, runs: cacheAccount, configured: off,
			replaced: true, bucket: "", account: otherAccount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []k8sruntime.Object{cacheObjectOf(t, configuredRuntime(t, tc.written), cacheRepo, "uid-cache", modeRunning, "")}
			options := []rigOption{withOptions(tc.configured)}
			if tc.runs == "" {
				options = append(options, withoutController())
			} else {
				live := cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodRunning)
				live.Spec.ServiceAccountName = tc.runs
				objects = append(objects, live)
			}
			g := newRig(t, objects, options...)
			before := g.sandbox(name)
			g.clearActions()
			ensure(t, g)
			if !tc.replaced {
				if writes := podWrites(g); len(writes) > 0 {
					t.Fatalf("a cache bound as configured was written: %q", writes)
				}
				return
			}
			if !replaced(t, g) {
				t.Fatalf("the ensure did not make the cache's pod again: writes %q", podWrites(g))
			}
			after := g.sandbox(name)
			if after == nil || after.UID != before.UID || !jsonEqual(t, after.Spec.VolumeClaimTemplates, before.Spec.VolumeClaimTemplates) {
				t.Fatalf("the cache Sandbox is %+v, want %s kept with its volume claim template %+v", after, before.UID, before.Spec.VolumeClaimTemplates)
			}
			if bucket, account := cacheBinding(t, after.Spec.PodTemplate.Spec); bucket != tc.bucket || account != tc.account {
				t.Errorf("the replacement's pod template is told bucket %q and runs as %s, want %q and %s", bucket, account, tc.bucket, tc.account)
			}
			if tc.runs != "" {
				g.eventually("the replacement pod on "+tc.account, func() bool {
					pod := g.r.storedPod(name)
					return pod != nil && ownedBy(pod, before.UID) && pod.DeletionTimestamp == nil && serviceAccountOr(pod.Spec.ServiceAccountName) == tc.account
				})
			}
			g.clearActions()
			ensure(t, g)
			if writes := podWrites(g); len(writes) > 0 {
				t.Errorf("the ensure after the replacement wrote %q, want nothing: the replacement is bound as configured", writes)
			}
		})
	}

	t.Run("a pod sending a tar is made again only once it sends none", func(t *testing.T) {
		var streams atomic.Int64
		streams.Store(1)
		served := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(workspace.CacheRepositoryHeader, cacheRepo.String())
			n := int(streams.Load())
			_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: cacheRepo.String(), Streams: &n})
		}))
		t.Cleanup(served.Close)
		live := cachePod(cacheRepo, "uid-pod-old", "uid-cache", testImage, corev1.PodRunning)
		live.Spec.ServiceAccountName, live.Status.PodIP = otherAccount, "10.0.0.5"
		logs := &logBuffer{}
		written := cacheObjectOf(t, configuredRuntime(t, off), cacheRepo, "uid-cache", modeRunning, "")
		g := newRig(t, []k8sruntime.Object{written, live}, withLog(logs), withOptions(on))
		g.r.cacheDial = (&podNetwork{routes: map[string]string{"10.0.0.5:8418": served.Listener.Addr().String()}}).dial
		g.clearActions()
		ensure(t, g)
		if writes := podWrites(g); len(writes) > 0 {
			t.Errorf("turning the bucket on replaced a cache pod sending a tar: %q", writes)
		}
		if !strings.Contains(logs.String(), "repository cache pod is replaced once it serves no stream") {
			t.Errorf("the waiting replacement was not logged:\n%s", logs)
		}
		streams.Store(0)
		g.clearActions()
		ensure(t, g)
		if !replaced(t, g) {
			t.Fatalf("a cache pod sending no tar was not made again under the bucket: writes %q", podWrites(g))
		}
		if bucket, account := cacheBinding(t, g.sandbox(name).Spec.PodTemplate.Spec); bucket != testBucket || account != cacheAccount {
			t.Errorf("the replacement's pod template is told bucket %q and runs as %s, want %s and %s", bucket, account, testBucket, cacheAccount)
		}
	})
}

// A repository of the cache set whose cache cannot be made — its token cannot be minted — is logged
// as the sweep's warning naming the cache and tried at the next sweep; ReconcileOrphans answers
// nil, since its error holds the claims' launches back at boot and each launch ensures its own
// repository's cache.
func TestACacheThatCannotBeMadeNeverFailsTheReconciliation(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs), withOptions(func(o *Options) {
		o.Tokens = refusingTokens{}
		o.CacheRepositories = func() []ghrepo.Repository { return []ghrepo.Repository{cacheRepo} }
	}))
	for range 2 {
		if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
			t.Fatalf("ReconcileOrphans with a cache that cannot be made = %v, want nil", err)
		}
	}
	name := CacheSandboxName(testProject, cacheRepo)
	warnings := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "repository cache sweep failed") && strings.Contains(line, name) && strings.Contains(line, "GitHub is down") {
			warnings++
		}
	}
	if warnings != 2 {
		t.Errorf("%d sweep warnings name %s and the mint's failure, want one per sweep:\n%s", warnings, name, logs)
	}
}

// A tree's cleanup lists the tree's Sandboxes, which a cache never is: it survives.
func TestCleanupTreeNeverListsACache(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	g.spawn(rootSpec(t))
	g.store.close(testTree)
	if err := g.r.CleanupTree(g.ctx, testTree); err != nil {
		t.Fatalf("CleanupTree: %v", err)
	}
	if g.sandbox(SandboxName(rootToken)) != nil {
		t.Fatal("the tree's own Sandbox survived its cleanup")
	}
	if g.sandbox(CacheSandboxName(testProject, cacheRepo)) == nil {
		t.Error("the tree's cleanup deleted the repository cache")
	}
}

// The gh refresher walks the issue and controller pods; a cache pod, which has no role, is passed
// over with nothing logged and nothing written.
func TestGitHubRefreshSkipsCachePods(t *testing.T) {
	t.Parallel()
	logs := &logBuffer{}
	g := newRig(t, nil, withLog(logs))
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	before := logs.String()
	g.clearActions()
	g.r.refreshGitHubCredentials(g.ctx)
	if writes := g.sandboxWrites(); len(writes) != 0 {
		t.Errorf("the gh refresh wrote %q", writes)
	}
	if after := strings.TrimPrefix(logs.String(), before); after != "" {
		t.Errorf("the gh refresh logged over a cache pod:\n%s", after)
	}
}

// mountsOf is a container's mounts of the issue volume, as "<path> <subPath> <ro|rw>".
func mountsOf(c corev1.Container) []string {
	var mounts []string
	for _, m := range c.VolumeMounts {
		if m.Name != issueVolume {
			continue
		}
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		mounts = append(mounts, m.MountPath+" "+m.SubPath+" "+mode)
	}
	slices.Sort(mounts)
	return mounts
}

// The feed lives on the issue's own volume, under the subPath feed: workspace-fetch mounts that
// alone, read-write, and no other part of the volume; workspace-init and the roles container
// mount the volume whole and the feed again, read-only, over it, so no agent can write the feed the
// token-holding container reads. No emptyDir feed is left.
func TestTheFeedIsOnTheIssueVolumeAndOnlyTheFetchWritesIt(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	loc := g.spawn(rootSpec(t))
	spec := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Spec
	feed := FeedDir + " " + feedSubPath
	for _, c := range spec.InitContainers {
		switch c.Name {
		case fetchContainer:
			if got := mountsOf(c); !slices.Equal(got, []string{feed + " rw"}) {
				t.Errorf("workspace-fetch mounts the issue volume %q, want the feed alone, read-write", got)
			}
		case initContainer:
			if got := mountsOf(c); !slices.Equal(got, []string{TreeRoot + "  rw", feed + " ro"}) {
				t.Errorf("workspace-init mounts the issue volume %q, want it whole and the feed read-only", got)
			}
		}
	}
	for _, c := range spec.Containers {
		if got := mountsOf(c); !slices.Contains(got, feed+" ro") || !slices.Contains(got, TreeRoot+"  rw") || slices.Contains(got, feed+" rw") {
			t.Errorf("container %s mounts the issue volume %q, want it whole and the feed read-only", c.Name, got)
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == "feed" {
			t.Errorf("the pod still has a volume named feed: %+v", v)
		}
	}
	if FeedDir != TreeRoot+"/"+feedSubPath {
		t.Errorf("FeedDir is %s, want the feed subPath under TreeRoot, where every container sees it", FeedDir)
	}
}

// fetchEnv is the workspace-fetch container's environment in the pod template of the Sandbox name.
func (g *rig) fetchEnv(name string) map[string]string {
	env := map[string]string{}
	for _, c := range g.sandbox(name).Spec.PodTemplate.Spec.InitContainers {
		if c.Name == fetchContainer {
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
		}
	}
	return env
}

// Every issue pod's fetch container is told its repository's cache — its Service's name, which
// resolves while the cache's pod is Ready and not before, so the fetch waits on it — and the cache's
// bearer token, as a file projected from the issue pod's own provisioning Secret, never an
// environment value. A launch makes the cache when the repository has none yet. The token reaches
// the fetch container alone, and is the cache's own. With the cache's bucket on, the fetch is told
// and mounts exactly what it is told and mounts without it, and the issue pod and the controller's
// pod run as the account every pod runs as and are told no bucket: the bucket and the account that
// holds its grant are the cache pod's alone, and an issue pod holds no grant on the bucket.
func TestAnIssuePodsFetchIsToldItsRepositorysCacheAndItsToken(t *testing.T) {
	t.Parallel()
	// launched is what a launch gave an issue pod's fetch: its container and the provisioning
	// volume its token comes from.
	type launched struct {
		fetch        corev1.Container
		provisioning corev1.Volume
	}
	launch := func(t *testing.T, configured func(*Options)) launched {
		t.Helper()
		g := newRig(t, nil, withOptions(configured))
		repo := rootSpec(t).Repository
		loc := g.spawn(rootSpec(t))
		cache := CacheSandboxName(testProject, repo)
		if g.sandbox(cache) == nil {
			t.Fatalf("launching an issue of %s made no cache", repo)
		}
		bearer := string(g.secret(cacheSecretName(cache)).Data[cacheTokenKey])
		env := g.fetchEnv(loc.Sandbox.Name)
		if want := "http://" + cache + "." + testNamespace + ".svc:8418"; env["LEGION_REPOSITORY_CACHE_URL"] != want {
			t.Errorf("the fetch is told LEGION_REPOSITORY_CACHE_URL=%q, want %q", env["LEGION_REPOSITORY_CACHE_URL"], want)
		}
		if env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"] != ProvisionDir+"/"+cacheTokenKey {
			t.Errorf("the fetch is told LEGION_REPOSITORY_CACHE_TOKEN_FILE=%q", env["LEGION_REPOSITORY_CACHE_TOKEN_FILE"])
		}
		if want := rootSpec(t).DefaultBranch; want == "" || env["LEGION_DEFAULT_BRANCH"] != want {
			t.Errorf("the fetch is told LEGION_DEFAULT_BRANCH=%q, want the issue's recorded default %q", env["LEGION_DEFAULT_BRANCH"], want)
		}
		if issueSecret := g.secret(secretName(loc.Sandbox.Name)); issueSecret == nil || string(issueSecret.Data[cacheTokenKey]) != bearer || bearer == "" {
			t.Errorf("the issue pod's provisioning Secret holds %v, want the cache's bearer %q", issueSecret, bearer)
		}
		spec := g.sandbox(loc.Sandbox.Name).Spec.PodTemplate.Spec
		provisioning := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == provisionVolume })
		if provisioning < 0 {
			t.Fatalf("the issue pod has no %s volume, only %+v", provisionVolume, spec.Volumes)
		}
		if v := spec.Volumes[provisioning]; v.Secret == nil || !slices.ContainsFunc(v.Secret.Items, func(k corev1.KeyToPath) bool { return k.Key == cacheTokenKey }) {
			t.Errorf("the provisioning volume %+v projects no cache token", v)
		}
		for _, c := range append(slices.Clone(spec.InitContainers), spec.Containers...) {
			for _, e := range c.Env {
				if e.Value == bearer {
					t.Errorf("container %s is told the cache's token in %s", c.Name, e.Name)
				}
			}
			if c.Name != fetchContainer && slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == provisionVolume }) {
				t.Errorf("container %s mounts the provisioning Secret", c.Name)
			}
		}
		controller := g.sandbox(g.spawn(controllerSpec(t)).Sandbox.Name).Spec.PodTemplate.Spec
		for _, pod := range []struct {
			what string
			spec corev1.PodSpec
		}{{"the issue pod", spec}, {"the controller's pod", controller}} {
			if account := serviceAccountOr(pod.spec.ServiceAccountName); account != otherAccount {
				t.Errorf("%s runs as %s, want %s, the account every pod but the cache's runs as", pod.what, account, otherAccount)
			}
			for _, c := range slices.Concat(pod.spec.InitContainers, pod.spec.Containers) {
				for _, e := range c.Env {
					if e.Name == repocache.BucketEnv || e.Value == testBucket {
						t.Errorf("container %s of %s is told %s=%q: the bucket is the cache pod's alone", c.Name, pod.what, e.Name, e.Value)
					}
				}
			}
		}
		return launched{fetch: containerNamed(t, spec, fetchContainer), provisioning: spec.Volumes[provisioning]}
	}
	without := launch(t, sharedAccount)
	with := launch(t, withBucket(testBucket, cacheAccount))
	if !equality.Semantic.DeepEqual(with.fetch.Env, without.fetch.Env) {
		t.Errorf("with the cache's bucket on the fetch is told\n%+v\nwant what it is told without it\n%+v", with.fetch.Env, without.fetch.Env)
	}
	if !equality.Semantic.DeepEqual(with.fetch.VolumeMounts, without.fetch.VolumeMounts) || !equality.Semantic.DeepEqual(with.provisioning, without.provisioning) {
		t.Errorf("with the cache's bucket on the fetch mounts %+v from %+v, want what it mounts without it, %+v from %+v",
			with.fetch.VolumeMounts, with.provisioning, without.fetch.VolumeMounts, without.provisioning)
	}
}

// RepositoryCaches is every cache the stores hold, by repository, each with its volume claim's name
// — the claim the controller makes from its Sandbox's volume template, which an operator grows in
// place — and with what its /healthz answered at the last health read (on the gh refresher's
// tick): a Ready cache's head, bytes and last fetch, and its room on its volume with the time its
// repack last failed for lack of space, a cache that could not be read with why, and one not Ready
// with nothing read. A cache of a repository the set no longer holds carries when the sweep retires
// it.
func TestRepositoryCachesCarryTheirLastHealthRead(t *testing.T) {
	t.Parallel()
	fetched := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	repackFailed := time.Date(2026, 11, 19, 2, 15, 33, 0, time.UTC)
	volume, available := uint64(20<<30), uint64(107374182)
	var presented atomic.Value
	presented.Store("")
	served := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		presented.Store(r.Header.Get("Authorization"))
		w.Header().Set(workspace.CacheRepositoryHeader, "acme/widgets")
		_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: "acme/widgets", Head: "0123abcd", LastFetchAt: fetched, Bytes: 6612345678,
			VolumeBytes: &volume, AvailableBytes: &available, RepackOutOfSpaceAt: repackFailed})
	}))
	t.Cleanup(served.Close)
	network := &podNetwork{routes: map[string]string{"10.0.0.5:8418": served.Listener.Addr().String()}}
	gadgets := ghrepo.MustParse("acme/gadgets")
	var inSet atomic.Bool
	inSet.Store(true)
	g := newRig(t, nil, withOptions(func(o *Options) {
		o.CacheRepositories = func() []ghrepo.Repository {
			if inSet.Load() {
				return []ghrepo.Repository{cacheRepo, gadgets}
			}
			return []ghrepo.Repository{cacheRepo}
		}
	}))
	g.r.cacheDial = network.dial
	for _, repo := range []ghrepo.Repository{cacheRepo, gadgets} {
		if err := g.r.EnsureRepositoryCache(g.ctx, repo); err != nil {
			t.Fatal(err)
		}
		name := CacheSandboxName(testProject, repo)
		g.eventually("the cache pod of "+repo.String(), func() bool { return g.r.storedPod(name) != nil })
	}
	g.update(g.pod(CacheSandboxName(testProject, cacheRepo)), func(p *corev1.Pod) {
		p.Status.PodIP = "10.0.0.5"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	g.eventually("the cache ready", func() bool { status, _ := g.r.RepositoryCache(cacheRepo); return status.Ready })
	inSet.Store(false)
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	g.eventually("acme/gadgets marked", func() bool {
		s, _ := g.r.storedSandbox(CacheSandboxName(testProject, gadgets))
		return s != nil && s.Annotations[annotationCacheUnusedAt] != ""
	})

	before := g.r.RepositoryCaches()
	if len(before) != 2 || before[0].Health != nil || before[0].HealthError != "" {
		t.Fatalf("RepositoryCaches before a health read = %+v, want two caches and nothing read", before)
	}
	g.r.readCacheHealth(g.ctx)
	if presented.Load() != "" {
		t.Errorf("the health read presented %q, want no credential", presented.Load())
	}
	caches := g.r.RepositoryCaches()
	if len(caches) != 2 || caches[0].Repo != gadgets || caches[1].Repo != cacheRepo {
		t.Fatalf("RepositoryCaches = %+v, want acme/gadgets then acme/widgets", caches)
	}
	for _, cache := range caches {
		stored, err := g.r.storedSandbox(cache.Sandbox)
		if err != nil || stored == nil || len(stored.Spec.VolumeClaimTemplates) != 1 {
			t.Fatalf("the cache Sandbox %s is %+v (%v), want it stored with its one volume claim template", cache.Sandbox, stored, err)
		}
		if want := stored.Spec.VolumeClaimTemplates[0].Metadata.Name + "-" + stored.Name; cache.Claim != want {
			t.Errorf("the cache of %s names its claim %q, want %q: the claim the controller makes from its Sandbox's volume template", cache.Repo, cache.Claim, want)
		}
	}
	if gadgetsCache := caches[0]; gadgetsCache.Ready || gadgetsCache.Health != nil || gadgetsCache.HealthError != "" || gadgetsCache.RetainedUntil.IsZero() {
		t.Errorf("the not-Ready cache outside the set is %+v, want nothing read and its retention", gadgetsCache)
	}
	widgets := caches[1]
	if !widgets.Ready || widgets.Health == nil || widgets.Health.Head != "0123abcd" || widgets.Health.Bytes != 6612345678 || !widgets.Health.LastFetchAt.Equal(fetched) || widgets.HealthError != "" {
		t.Errorf("the Ready cache is %+v (health %+v), want its /healthz", widgets, widgets.Health)
	}
	if room := widgets.Health; room == nil || room.VolumeBytes == nil || *room.VolumeBytes != volume || room.AvailableBytes == nil || *room.AvailableBytes != available ||
		room.VolumeError != "" || !room.FetchOutOfSpaceAt.IsZero() || !room.RepackOutOfSpaceAt.Equal(repackFailed) {
		t.Errorf("the Ready cache's health is %+v, want its room on its volume and its repack's failure for lack of space as /healthz answered them", room)
	}

	served.Close()
	g.r.readCacheHealth(g.ctx)
	if widgets := g.r.RepositoryCaches()[1]; widgets.Health != nil || !strings.Contains(widgets.HealthError, "connection refused") {
		t.Errorf("a cache whose /healthz cannot be read is %+v, want the read's failure and no health", widgets)
	}
}

// podNetwork is a cacheDial for a daemon outside the cluster: it reaches pod addresses, each
// served by the listener routes names, and resolves no name at all, as a host with no cluster DNS
// does. It records every address dialed.
type podNetwork struct {
	routes map[string]string
	mu     sync.Mutex
	dialed []string
}

func (n *podNetwork) dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	n.dialed = append(n.dialed, address)
	n.mu.Unlock()
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) == nil {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	to, routed := n.routes[address]
	if !routed {
		return nil, fmt.Errorf("dial %s: no route to host", address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, to)
}

func (n *podNetwork) addresses() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.dialed)
}

// healthServer is a cache's /healthz at an httptest listener, naming repo and recording the
// Authorization header of every request it answers.
func healthServer(t *testing.T, repo string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var credentials []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set(workspace.CacheRepositoryHeader, repo)
		_ = json.NewEncoder(w).Encode(repocache.Health{Ready: true, Repo: repo, Head: "0123abcd"})
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(credentials)
	}
}

// readyCache makes cacheRepo's cache and marks its pod Ready at podIP, returning the Sandbox's name.
func readyCache(t *testing.T, g *rig, podIP string) string {
	t.Helper()
	if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
		t.Fatal(err)
	}
	name := CacheSandboxName(testProject, cacheRepo)
	g.eventually("the cache pod", func() bool { return g.r.storedPod(name) != nil })
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status.PodIP = podIP
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	})
	g.eventually("the cache ready", func() bool { status, _ := g.r.RepositoryCache(cacheRepo); return status.Ready })
	return name
}

// A health read asks the cache pod at its address, as the store holds it, and carries no
// credential: /healthz holds nothing secret, so a daemon outside the cluster, which reaches pod
// addresses but resolves no cluster name, reads every cache's health. On the wire the request has
// no Authorization header. A read is the pod's it was made of: once that pod is replaced, its
// successor shows none until it is read.
func TestTheHealthReadAsksThePodAddressWithNoCredential(t *testing.T) {
	t.Parallel()
	served, credentials := healthServer(t, cacheRepo.String())
	g := newRig(t, nil)
	network := &podNetwork{routes: map[string]string{"10.0.0.5:8418": served.Listener.Addr().String()}}
	g.r.cacheDial = network.dial
	name := readyCache(t, g, "10.0.0.5")

	g.r.readCacheHealth(g.ctx)

	if dialed := network.addresses(); !slices.Equal(dialed, []string{"10.0.0.5:8418"}) {
		t.Errorf("the health read dialed %q, want the pod's address alone", dialed)
	}
	if got := credentials(); !slices.Equal(got, []string{""}) {
		t.Errorf("the cache's /healthz was asked with Authorization %q, want one request carrying none", got)
	}
	caches := g.r.RepositoryCaches()
	if len(caches) != 1 || caches[0].Health == nil || caches[0].Health.Head != "0123abcd" || caches[0].HealthError != "" {
		t.Fatalf("RepositoryCaches = %+v, want the cache's own health", caches)
	}
	g.update(g.pod(name), func(p *corev1.Pod) { p.UID = "uid-cache-pod-successor" })
	g.eventually("the successor pod", func() bool { pod := g.r.storedPod(name); return pod != nil && pod.UID == "uid-cache-pod-successor" })
	if caches := g.r.RepositoryCaches(); len(caches) != 1 || caches[0].Health != nil || caches[0].HealthError != "" {
		t.Errorf("RepositoryCaches after the pod was replaced = %+v, want no read shown for the successor", caches)
	}
}

// A stranger holding the cache pod's address, as a pod that took it once the cache's pod was gone
// would, is asked the health with no credential, so it learns nothing: the cache's token goes to
// no health read. Its answer names another repository, which the read refuses: the cache shows that
// read's failure, and no health, until the next.
func TestAStrangerAtTheCachePodsAddressGetsNoCredential(t *testing.T) {
	t.Parallel()
	stranger, credentials := healthServer(t, "acme/gadgets")
	g := newRig(t, nil)
	g.r.cacheDial = (&podNetwork{routes: map[string]string{"10.0.0.5:8418": stranger.Listener.Addr().String()}}).dial
	readyCache(t, g, "10.0.0.5")

	g.r.readCacheHealth(g.ctx)

	if got := credentials(); len(got) != 1 || got[0] != "" {
		t.Errorf("the stranger was asked with Authorization %q, want one request carrying none", got)
	}
	caches := g.r.RepositoryCaches()
	if len(caches) != 1 || caches[0].Health != nil || !strings.Contains(caches[0].HealthError, `naming the repository "acme/gadgets", not acme/widgets`) {
		t.Fatalf("RepositoryCaches = %+v, want the read refused for naming acme/gadgets and no health", caches)
	}
}

// cacheService is the headless Service the Agent Sandbox controller makes for the cache Sandbox
// name with uid: controlled by it, and selecting its pod by the controller's hash of its name.
func cacheService(name string, uid types.UID) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace, Labels: map[string]string{sandboxNameHashLabel: sandboxNameHash(name)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: "Sandbox", Name: name, UID: uid, Controller: new(true)}},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{sandboxNameHashLabel: sandboxNameHash(name)}},
	}
}

// sandboxNameHash is the Agent Sandbox controller's NameHash (v1.0.3): the values below are what
// the controller's own function returns for these names, so the selector the daemon requires of a
// cache's Service is the one the controller writes.
func TestSandboxNameHashIsTheControllersNameHash(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"":  "811c9dc5",
		"a": "e40c292c",
		"legion-legion-cache-acme-widgets-d782c874": "7e13b444",
	} {
		if got := sandboxNameHash(name); got != want {
			t.Errorf("sandboxNameHash(%q) = %s, want the controller's %s", name, got, want)
		}
	}
}

// The issue pods' fetch reaches the cache at its Service with the cache's token, so the daemon
// hands an issue pod the cache's address only once that Service is the cache Sandbox's: controlled
// by it, by uid, selecting exactly its pod by the controller's hash of its name, and headless with
// no external name. A Service of the cache's name that is not is refused: one no Sandbox controls
// and without the controller's tracking label, which anyone who may create a Service could have
// made first and which the controller refuses to adopt; one another Sandbox controls; one selecting
// other pods; and an ExternalName Service planted with the tracking label, as the controller
// leaves it once it has adopted it — its own controller reference and the exact selector written,
// its type and external name kept. The launch waits on it, logging why, and fails at the boot
// timeout naming the Service, with no issue pod made and no token written for one; and legion
// state shows the cache refused, naming the Service and why.
func TestAnIssuePodGetsNoCacheAddressFromAServiceThatIsNotTheCaches(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit func(*corev1.Service)
		says string
	}{
		"a Service selecting other pods": {func(s *corev1.Service) { s.Spec.Selector = map[string]string{"app": "elsewhere"} }, "selects map[app:elsewhere]"},
		"a Service no Sandbox controls": {func(s *corev1.Service) {
			s.OwnerReferences, s.Labels = nil, map[string]string{"app": "elsewhere"}
		}, "has no controller"},
		"a Service another Sandbox controls": {func(s *corev1.Service) { s.OwnerReferences[0].UID = "uid-other" }, "(uid-other), not the cache's Sandbox (uid-cache)"},
		"an ExternalName Service the controller adopted": {func(s *corev1.Service) {
			s.Spec.Type, s.Spec.ClusterIP, s.Spec.ExternalName = corev1.ServiceTypeExternalName, "", "collector.attacker.example"
		}, "is a ExternalName Service"},
		"a ClusterIP Service whose maker wrote the controller reference": {func(s *corev1.Service) {
			s.Spec.Type, s.Spec.ClusterIP = corev1.ServiceTypeClusterIP, "10.0.0.196"
		}, "is a ClusterIP Service"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := rootSpec(t)
			cache := CacheSandboxName(testProject, spec.Repository)
			service := cacheService(cache, "uid-cache")
			tc.edit(service)
			logs := &logBuffer{}
			g := newRig(t, []k8sruntime.Object{cacheObject(t, spec.Repository, "uid-cache", modeRunning, ""), service}, withLog(logs))

			_, err := g.r.Spawn(g.ctx, spec)
			if err == nil || !strings.Contains(err.Error(), "the repository cache's Service "+cache) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Spawn = %v, want the launch refused naming the Service %s and %q", err, cache, tc.says)
			}
			issue := SandboxName(spec.Claim)
			if pod := g.pod(issue); pod != nil {
				t.Errorf("the refused launch made the issue pod %s", pod.Name)
			}
			if secret := g.secret(secretName(issue)); secret != nil {
				t.Errorf("the refused launch wrote the provisioning Secret %s: %v", secret.Name, slices.Sorted(maps.Keys(secret.Data)))
			}
			if !strings.Contains(logs.String(), "service="+cache) || !strings.Contains(logs.String(), tc.says) {
				t.Errorf("the launch logged %q, want its wait on the Service %s and why", logs.String(), cache)
			}
			status, found := g.r.RepositoryCache(spec.Repository)
			if !found || status.State != cacheRefused || status.Ready || status.Address != "" || !strings.Contains(status.Reason, "Service "+cache) || !strings.Contains(status.Reason, tc.says) {
				t.Errorf("RepositoryCache = %+v, %t; want it refused, naming the Service %s and %q", status, found, cache, tc.says)
			}
		})
	}
}

// While the Services list or watch is failing, the Service store may hold a Service that has since
// changed, so even the cache's own Service is not trusted: a launch waits, logging why, and fails
// at the boot timeout naming the Service and the stale store, with nothing written, and legion
// state shows the cache refused, saying its Service cannot be checked. Once a request succeeds the
// store is trusted again. The failure recorded here is what the Service informer's list or watch
// records when the API server refuses it.
func TestNoServiceIsTrustedWhileTheServiceWatchFails(t *testing.T) {
	t.Parallel()
	spec := rootSpec(t)
	cache := CacheSandboxName(testProject, spec.Repository)
	logs := &logBuffer{}
	g := newRig(t, []k8sruntime.Object{cacheObject(t, spec.Repository, "uid-cache", modeRunning, ""), cacheService(cache, "uid-cache")}, withLog(logs))
	g.r.serviceFeed.record(errors.New(`services is forbidden: User "legion-daemon" cannot watch resource "services"`), time.Now())
	says := "cannot be checked: the Service store may be stale"

	_, err := g.r.Spawn(g.ctx, spec)
	if err == nil || !strings.Contains(err.Error(), "the repository cache's Service "+cache+" "+says) {
		t.Fatalf("Spawn = %v, want the launch refused naming the Service %s and %q", err, cache, says)
	}
	issue := SandboxName(spec.Claim)
	if pod, secret := g.pod(issue), g.secret(secretName(issue)); pod != nil || secret != nil {
		t.Errorf("the refused launch made pod %v and Secret %v, want neither", pod != nil, secret != nil)
	}
	if !strings.Contains(logs.String(), "service="+cache) || !strings.Contains(logs.String(), says) {
		t.Errorf("the launch logged %q, want its wait on the Service %s and why", logs.String(), cache)
	}
	if status, _ := g.r.RepositoryCache(spec.Repository); status.State != cacheRefused || status.Address != "" || !strings.Contains(status.Reason, "its Service "+cache+" "+says) {
		t.Errorf("RepositoryCache = %+v, want it refused, its Service unchecked", status)
	}

	g.r.serviceFeed.record(nil, time.Now())
	if refusal, made := g.r.cacheServiceRefusal(cache, "uid-cache"); refusal != "" || !made {
		t.Errorf("cacheServiceRefusal once the watch is fed again = %q, %t; want the cache's own Service", refusal, made)
	}
}

package sandbox

import (
	"io"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// storeRuntime is the runtime of goldenOptions under the session store store, with edits applied.
func storeRuntime(t *testing.T, store ompsessions.Store, edits ...func(*Options)) *Runtime {
	t.Helper()
	return sandboxRuntime(t, SandboxConfig{}, append([]func(*Options){func(o *Options) { o.SessionStore = store }}, edits...)...)
}

// projectedTokens are the volumes of pod that project a ServiceAccount token, by the token's
// audience.
func projectedTokens(pod corev1.PodSpec) map[string][]string {
	tokens := map[string][]string{}
	for _, volume := range pod.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if token := source.ServiceAccountToken; token != nil {
				tokens[token.Audience] = append(tokens[token.Audience], volume.Name)
			}
		}
	}
	return tokens
}

// mountsNamed are the mounts of the volume called name in every container of pod, init containers
// included, each as `<container> <path>`, with ` read-only` after a read-only one.
func mountsNamed(pod corev1.PodSpec, name string) []string {
	var mounted []string
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		for _, m := range c.VolumeMounts {
			if m.Name != name {
				continue
			}
			at := c.Name + " " + m.MountPath
			if m.ReadOnly {
				at += " read-only"
			}
			mounted = append(mounted, at)
		}
	}
	return mounted
}

// checkNoSessionWriter fails the test when pod carries any part of the session writer token: its
// volume, a projected token of testWriterAudience, a mount of it or at or under SessionWriterDir in
// any container, or the writer audience flag on any container's command.
func checkNoSessionWriter(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	for _, volume := range pod.Volumes {
		if volume.Name == sessionWriterTokenVolume {
			t.Errorf("the pod carries the volume %+v", volume)
		}
	}
	if held := projectedTokens(pod)[testWriterAudience]; len(held) > 0 {
		t.Errorf("the pod's volumes %v project a token of the writer audience", held)
	}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		for _, m := range c.VolumeMounts {
			if m.Name == sessionWriterTokenVolume || m.MountPath == SessionWriterDir || strings.HasPrefix(m.MountPath, SessionWriterDir+"/") {
				t.Errorf("%s mounts %+v", c.Name, m)
			}
		}
		if slices.Contains(c.Command, "--session-writer-audience") {
			t.Errorf("%s runs %q, which carries --session-writer-audience", c.Name, c.Command)
		}
	}
}

// A pod whose session database names a writer audience carries one projected ServiceAccount token
// of that audience, session-writer-token: an hour long, the admission policy's cap, at `token`,
// readable by the pod's group (0440: the pod runs with an fsGroup). Its roles container alone
// mounts it, once and read-only, at RolesDir/session-writer, which makes the token the file the
// launcher reads under its --roles-dir; that is under RolesDir, which every role's namespace
// covers, and in no role's own directory there, which role-init binds back, nor in the role homes.
// No init container mounts it, and the launcher is told the audience right after the service
// account it impersonates with it. A store that names no writer audience renders none of the
// three: no such volume, no mount under SessionWriterDir, no flag.
func TestAnIssuePodMountsTheSessionWriterTokenWhereNoRoleSeesIt(t *testing.T) {
	pod := podOf(t, storeRuntime(t, cloudSQLWriterStore), rootSpec(t))
	expiry := int64(3600)
	want := corev1.Volume{Name: "session-writer-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		DefaultMode: new(int32(0o440)),
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: testWriterAudience, ExpirationSeconds: &expiry, Path: "token",
		}}},
	}}}
	var held []corev1.Volume
	for _, volume := range pod.Volumes {
		if volume.Name == want.Name {
			held = append(held, volume)
		}
	}
	if !reflect.DeepEqual(held, []corev1.Volume{want}) {
		t.Errorf("the pod's writer token volumes are %+v, want exactly %+v", held, want)
	}
	if got := projectedTokens(pod)[testWriterAudience]; !slices.Equal(got, []string{want.Name}) {
		t.Errorf("the volumes projecting a token of the writer audience are %v, want %s alone", got, want.Name)
	}
	if got, wantAt := mountsNamed(pod, want.Name), []string{workerContainer + " /var/run/legion/roles/session-writer read-only"}; !slices.Equal(got, wantAt) {
		t.Errorf("the writer token is mounted at %v, want %v", got, wantAt)
	}
	roles := containerNamed(t, pod, workerContainer)
	rolesDir, _ := flagValue(roles.Command, "--roles-dir")
	tokenFile := filepath.Join(rolesDir, launcher.SessionWriterDirName, launcher.SessionWriterTokenFile)
	mount := mountHolding(roles, tokenFile)
	if mount == nil || mount.Name != want.Name || strings.TrimPrefix(tokenFile, mount.MountPath+"/") != want.Projected.Sources[0].ServiceAccountToken.Path {
		t.Fatalf("the launcher's writer token file %s is held by %+v, want the token %s projects", tokenFile, mount, want.Name)
	}
	if strings.HasPrefix(mount.MountPath, RoleHomesDir+"/") {
		t.Errorf("the writer token is mounted at %s, in the role homes", mount.MountPath)
	}
	for _, role := range claim.Roles {
		if dir := roleDir(role); mount.MountPath == dir || strings.HasPrefix(mount.MountPath, dir+"/") {
			t.Errorf("the writer token is mounted at %s, in %s's own directory %s, which role-init binds back", mount.MountPath, role, dir)
		}
	}
	if flags := []string{
		"--session-impersonate-service-account", cloudSQLWriterStore.Database.ImpersonateServiceAccount,
		"--session-writer-audience", testWriterAudience,
	}; !containsSequence(roles.Command, flags) {
		t.Errorf("the launcher runs %q, want %q", roles.Command, flags)
	}
	for name, store := range map[string]ompsessions.Store{
		"postgres": postgresStore, "cloudsql-iam": cloudSQLStore, "cloudsql-iam impersonating a service account": cloudSQLImpersonatingStore,
	} {
		t.Run("without a writer audience under "+name, func(t *testing.T) {
			checkNoSessionWriter(t, podOf(t, storeRuntime(t, store), rootSpec(t)))
		})
	}
}

// A microVM issue pod whose block names the metadata endpoint and whose session database names a
// writer audience carries both tokens its launcher exchanges, each in a volume of its own for an
// audience of its own — the metadata endpoint's for the Workload Identity pool, the session
// proxy's for the writer audience — and no other projected token, since the admission policy
// admits one volume per audience. Each is mounted read-only in the roles container at its own
// directory of RolesDir, and the launcher is told both.
func TestAMicroVMIssuePodCarriesTheWriterTokenBesideTheMetadataEndpointsToken(t *testing.T) {
	r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}, func(o *Options) { o.SessionStore = cloudSQLWriterStore })
	pod := podOf(t, r, rootSpec(t))
	want := map[string][]string{testWorkloadPool: {workloadIdentityTokenVolume}, testWriterAudience: {sessionWriterTokenVolume}}
	if got := projectedTokens(pod); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("the pod's projected tokens are %v, want exactly %v", got, want)
	}
	for volume, at := range map[string]string{workloadIdentityTokenVolume: WorkloadIdentityDir, sessionWriterTokenVolume: SessionWriterDir} {
		if got, wantAt := mountsNamed(pod, volume), []string{workerContainer + " " + at + " read-only"}; !slices.Equal(got, wantAt) {
			t.Errorf("%s is mounted at %v, want %v", volume, got, wantAt)
		}
	}
	command := containerNamed(t, pod, workerContainer).Command
	for flag, value := range map[string]string{"--metadata-workload-pool": testWorkloadPool, "--session-writer-audience": testWriterAudience} {
		if got, ok := flagValue(command, flag); !ok || got != value {
			t.Errorf("the launcher runs %q, want %s %s", command, flag, value)
		}
	}
}

// A microVM issue pod under keep_volume, whose disk is the claim its Sandbox owns and keeps across
// its pods (keptVolume), carries the writer token exactly as a microVM pod without it does: the
// Sandbox its launch makes owns the kept claim, and its pod template has one projected token of the
// writer audience beside the metadata endpoint's, mounted once, read-only, in the roles container
// at SessionWriterDir under RolesDir, which no role's namespace shows, and in no init container,
// with the launcher told the audience. The same pod of a store that names no writer audience
// carries none of it. And the writer audience is part of the session store the kept pod carries
// for its life, as it is of an unkept one: one built without it under a store that names it, one
// built with it under a store that names none, and one built with another have moved, and one
// built under the very store a daemon reads has not.
func TestAKeptVolumeMicroVMIssuePodCarriesTheWriterTokenUnderRolesDir(t *testing.T) {
	kept := SandboxConfig{Type: SandboxMicroVM, MicroVM: keptMicroVM()}
	r := sandboxRuntime(t, kept, func(o *Options) { o.SessionStore = cloudSQLWriterStore })
	s := r.sandboxManifest(launchOf(t, r, rootSpec(t)))
	if !ownsKeptVolume(&s) {
		t.Fatalf("the Sandbox claims %+v, not the kept volume, so this case proves nothing", s.Spec.VolumeClaimTemplates)
	}
	pod := s.Spec.PodTemplate.Spec
	want := map[string][]string{testWorkloadPool: {workloadIdentityTokenVolume}, testWriterAudience: {sessionWriterTokenVolume}}
	if got := projectedTokens(pod); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("the kept-volume pod's projected tokens are %v, want exactly %v", got, want)
	}
	if got, wantAt := mountsNamed(pod, sessionWriterTokenVolume), []string{workerContainer + " " + SessionWriterDir + " read-only"}; !slices.Equal(got, wantAt) {
		t.Errorf("%s is mounted at %v, want %v", sessionWriterTokenVolume, got, wantAt)
	}
	if got, ok := flagValue(containerNamed(t, pod, workerContainer).Command, "--session-writer-audience"); !ok || got != testWriterAudience {
		t.Errorf("the kept-volume pod's launcher is told the writer audience %q (%t), want %s", got, ok, testWriterAudience)
	}
	t.Run("without a writer audience", func(t *testing.T) {
		r := sandboxRuntime(t, kept, func(o *Options) { o.SessionStore = cloudSQLImpersonatingStore })
		s := r.sandboxManifest(launchOf(t, r, rootSpec(t)))
		if !ownsKeptVolume(&s) {
			t.Fatalf("the Sandbox claims %+v, not the kept volume, so this case proves nothing", s.Spec.VolumeClaimTemplates)
		}
		checkNoSessionWriter(t, s.Spec.PodTemplate.Spec)
	})
	t.Run("a writer audience added, dropped or changed moves the kept pod", func(t *testing.T) {
		another := cloudSQLWriterStore
		another.Database.WriterAudience = strings.TrimSuffix(testWriterAudience, "legion-cluster") + "legion-cluster-next"
		if !ompsessions.ValidWriterAudience(another.Database.WriterAudience) {
			t.Fatalf("the other writer audience %q is no writer audience", another.Database.WriterAudience)
		}
		none := sandboxRuntime(t, kept, func(o *Options) { o.SessionStore = cloudSQLImpersonatingStore })
		other := sandboxRuntime(t, kept, func(o *Options) { o.SessionStore = another })
		spec := rootSpec(t)
		pod := func(r *Runtime) *corev1.Pod { return &corev1.Pod{Spec: podOf(t, r, spec)} }
		for name, tc := range map[string]struct {
			r     *Runtime
			pod   *corev1.Pod
			moved bool
		}{
			"a kept pod without a writer audience under one": {r, pod(none), true},
			"a kept pod with a writer audience under none":   {none, pod(r), true},
			"a kept pod under another writer audience":       {other, pod(r), true},
			"a kept pod under the same writer audience":      {r, pod(r), false},
		} {
			t.Run(name, func(t *testing.T) {
				store, moved := tc.r.movedSessionStore(tc.pod)
				if moved != tc.moved {
					t.Errorf("movedSessionStore = %+v, %t; want moved %t", store, moved, tc.moved)
				}
				drifted := tc.r.driftedInPod(tc.pod, spec.Role)
				if got := slices.ContainsFunc(drifted, func(m movedAddress) bool { return m.where == "the session store" }); got != tc.moved {
					t.Errorf("driftedInPod = %v, want the session store moved %t", drifted, tc.moved)
				}
			})
		}
	})
}

// Under a writer audience the controller's pod runs the launcher's roles-dir form for its one role,
// laid out as an issue pod lays out each of its roles: the command names the roles directory and
// the controller, and neither --token-file nor --private-dir; the controller's own volumes are
// mounted under its directory of RolesDir, the launcher token and gh files read-only; the role
// homes volume, an emptyDir on the node's disk, at RoleHomesDir; the writer token read-only at
// SessionWriterDir alone; the broker's and the exchange's tokens at their shared paths; and what
// belongs under the home under HomeMountsDir. Nothing is mounted at the fixed paths the single-role
// form mounts the controller's own volumes at, which role-init binds the controller's own back at.
func TestTheControllersPodRunsTheRolesDirFormUnderAWriterAudience(t *testing.T) {
	opts := goldenOptions()
	opts.SessionStore = cloudSQLWriterStore
	opts.AgentSecrets = exchangeEnrolled()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	l := launchOf(t, r, controllerSpec(t))
	pod := r.podTemplate(l).Spec
	if len(pod.InitContainers) != 0 || len(pod.Containers) != 1 {
		t.Fatalf("the controller's pod runs init containers %v and containers %v, want its one launcher alone", pod.InitContainers, pod.Containers)
	}
	c := containerNamed(t, pod, string(claim.RoleController))
	database := cloudSQLWriterStore.Database
	if want := []string{
		"/opt/legion/bin/legion", "launcher", "--connect", opts.StreamURL, "--sandbox", l.name,
		"--roles-dir", "/var/run/legion/roles", "--role", "controller", "--stop-grace", "30s",
		"--session-instance", database.Instance, "--session-database", database.Name, "--session-user", database.User,
		"--session-impersonate-service-account", database.ImpersonateServiceAccount, "--session-writer-audience", testWriterAudience,
	}; !slices.Equal(c.Command, want) {
		t.Errorf("the controller's container runs %q, want %q", c.Command, want)
	}
	byPath := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		if _, twice := byPath[m.MountPath]; twice {
			t.Errorf("the controller's container mounts %s twice", m.MountPath)
		}
		byPath[m.MountPath] = m
	}
	for _, mount := range []struct {
		path, volume string
		readOnly     bool
	}{
		{"/var/run/legion/roles/controller/launcher", "launcher-controller", true},
		{"/var/run/legion/roles/controller/gh", "gh-controller", true},
		{"/var/run/legion/roles/controller/private", "private-controller", false},
		{"/var/run/legion/roles/controller/state", "state-controller", false},
		{"/var/run/legion/roles/controller/agent-secrets", "agent-secrets-key-controller", false},
		{"/var/run/legion/roles/.homes", roleHomesVolume, false},
		{"/var/run/legion/roles/session-writer", sessionWriterTokenVolume, true},
		{"/var/run/legion/home/.config", configVolume, false},
		{TreeRoot, issueVolume, false},
		{AgentSecretsTokenDir, agentSecretsTokenVolume, true},
		{AgentSecretsExchangeDir, agentSecretsExchangeVolume, true},
	} {
		if got, ok := byPath[mount.path]; !ok || got.Name != mount.volume || got.ReadOnly != mount.readOnly || got.SubPath != "" {
			t.Errorf("the controller's container mounts %+v at %s, want volume %s read-only %t", got, mount.path, mount.volume, mount.readOnly)
		}
	}
	if got, want := mountsNamed(pod, sessionWriterTokenVolume), []string{"controller /var/run/legion/roles/session-writer read-only"}; !slices.Equal(got, want) {
		t.Errorf("the writer token is mounted at %v, want %v", got, want)
	}
	for _, fixed := range []string{"/var/run/legion/launcher", "/var/run/legion/private", "/var/run/legion/state", "/var/run/legion/gh", "/var/run/legion/agent-secrets"} {
		if m := mountHolding(c, fixed); m != nil {
			t.Errorf("the controller's container mounts %s at %s, which only the controller's namespace binds (role-init)", m.Name, m.MountPath)
		}
	}
	for _, m := range c.VolumeMounts {
		if m.MountPath == podHome || strings.HasPrefix(m.MountPath, podHome+"/") {
			t.Errorf("the controller's container mounts %s at %s, under the home its namespace overlays: it belongs under %s", m.Name, m.MountPath, HomeMountsDir)
		}
	}
	var homes *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == roleHomesVolume {
			homes = &pod.Volumes[i]
		}
	}
	if homes == nil || homes.EmptyDir == nil || homes.EmptyDir.Medium != corev1.StorageMediumDefault {
		t.Errorf("the controller's role homes volume is %+v, want an emptyDir on the node's disk", homes)
	}
	if got := projectedTokens(pod)[testWriterAudience]; !slices.Equal(got, []string{sessionWriterTokenVolume}) {
		t.Errorf("the volumes projecting a token of the writer audience are %v, want %s alone", got, sessionWriterTokenVolume)
	}
}

// Under a writer audience the controller's pod runs the roles-dir form in the project's sandbox, as
// the single-role form does (INFRA-4567). In a microVM whose block names the metadata endpoint,
// its launcher is told --microvm and the endpoint's pool and identity provider, and the endpoint's
// token is mounted read-only once, in the controller's container, at WorkloadIdentityDir: the
// workload-identity directory under the --roles-dir it is told, where the roles-dir form's launcher
// reads the token (the launcher's servedMetadata), and under RolesDir, which no role's namespace
// shows. A gVisor controller pod in that form carries none of them, whether the project names no
// sandbox or names gVisor beside a microvm block that names the endpoint.
func TestTheControllersRolesDirPodIsToldItsMicroVMAndMountsTheMetadataTokenUnderRolesDir(t *testing.T) {
	writer := func(o *Options) { o.SessionStore = cloudSQLWriterStore }
	rolesDirCommand := func(t *testing.T, pod corev1.PodSpec) ([]string, string) {
		t.Helper()
		command := containerNamed(t, pod, runtime.ControllerPodContainer).Command
		rolesDir, ok := flagValue(command, "--roles-dir")
		if !ok {
			t.Fatalf("the controller's launcher runs %q, not the roles-dir form, so this case proves nothing", command)
		}
		return command, rolesDir
	}
	t.Run("in a microVM", func(t *testing.T) {
		r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}, writer)
		pod := podOf(t, r, controllerSpec(t))
		command, rolesDir := rolesDirCommand(t, pod)
		if !slices.Contains(command, "--microvm") {
			t.Errorf("the controller's launcher runs %q, want --microvm", command)
		}
		for flag, value := range map[string]string{"--metadata-workload-pool": testWorkloadPool, "--metadata-identity-provider": testIdentityProvider} {
			if got, ok := flagValue(command, flag); !ok || got != value {
				t.Errorf("the controller's launcher runs %q, want %s %s", command, flag, value)
			}
		}
		if read := filepath.Join(rolesDir, launcher.WorkloadIdentityDirName); read != WorkloadIdentityDir {
			t.Fatalf("the roles-dir launcher reads the token under %s, and the pod mounts it at %s", read, WorkloadIdentityDir)
		}
		want := []string{runtime.ControllerPodContainer + " " + WorkloadIdentityDir + " read-only"}
		if got := mountsNamed(pod, workloadIdentityTokenVolume); !slices.Equal(got, want) {
			t.Errorf("%s is mounted at %v, want %v", workloadIdentityTokenVolume, got, want)
		}
	})
	for name, block := range map[string]SandboxConfig{
		"in a project that names no sandbox":      {},
		"in gVisor beside an inert microvm block": {Type: SandboxGVisor, MicroVM: testMicroVM()},
	} {
		t.Run(name, func(t *testing.T) {
			r := sandboxRuntime(t, block, writer)
			pod := podOf(t, r, controllerSpec(t))
			if command, _ := rolesDirCommand(t, pod); slices.Contains(command, "--microvm") {
				t.Errorf("the gVisor controller's launcher runs %q, with --microvm", command)
			}
			checkNoMetadataEndpoint(t, pod)
		})
	}
}

// The goldens pin every byte of the two Sandboxes a writer audience changes: an issue pod, which
// gains the writer token and its launcher the audience, and the controller's pod, which runs the
// roles-dir form. Each is sent in fields the CRD declares. The controller goldens without the
// writer audience are TestManifestGoldens', unchanged by it.
func TestTheSessionWriterGoldens(t *testing.T) {
	r := storeRuntime(t, cloudSQLWriterStore)
	schema := sandboxSchema(t)
	for name, spec := range map[string]runtime.SpawnSpec{"root-session-writer": rootSpec(t), "controller-session-writer": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			manifest := manifestOf(t, r, spec)
			checkGolden(t, name, manifest)
			if found := schemaViolations(manifest, schema, ""); len(found) > 0 {
				t.Fatalf("the manifest has fields the CRD does not declare as sent:\n%s", strings.Join(found, "\n"))
			}
		})
	}
}

// The writer audience is part of the session store a pod carries for its life: its launcher was
// told it once, at the pod's creation, and impersonates the service account with that audience's
// token for the pod's life. So a pod built without one under a store that names one, one built with
// one under a store that names none, and one built with another have each moved, an issue pod and
// the controller's pod alike, whatever the layout the controller's was built in; and a pod built
// under the very store a daemon reads has not.
func TestAWriterAudienceAddedDroppedOrChangedMovesThePodsSessionStore(t *testing.T) {
	impersonating := storeRuntime(t, cloudSQLImpersonatingStore)
	writer := storeRuntime(t, cloudSQLWriterStore)
	another := cloudSQLWriterStore
	another.Database.WriterAudience = strings.TrimSuffix(testWriterAudience, "legion-cluster") + "legion-cluster-next"
	if !ompsessions.ValidWriterAudience(another.Database.WriterAudience) {
		t.Fatalf("the other writer audience %q is no writer audience", another.Database.WriterAudience)
	}
	other := storeRuntime(t, another)
	for kind, spec := range map[string]runtime.SpawnSpec{"an issue pod": workerSpec(t), "the controller's pod": controllerSpec(t)} {
		pod := func(r *Runtime) *corev1.Pod { return &corev1.Pod{Spec: podOf(t, r, spec)} }
		for name, tc := range map[string]struct {
			r     *Runtime
			pod   *corev1.Pod
			moved bool
		}{
			"a pod without a writer audience under one": {writer, pod(impersonating), true},
			"a pod with a writer audience under none":   {impersonating, pod(writer), true},
			"a pod under another writer audience":       {other, pod(writer), true},
			"a pod under the same writer audience":      {writer, pod(writer), false},
			"a pod under the same account and none":     {impersonating, pod(impersonating), false},
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				store, moved := tc.r.movedSessionStore(tc.pod)
				if moved != tc.moved {
					t.Errorf("movedSessionStore = %+v, %t; want moved %t", store, moved, tc.moved)
				}
				drifted := tc.r.driftedInPod(tc.pod, spec.Role)
				if got := slices.ContainsFunc(drifted, func(m movedAddress) bool { return m.where == "the session store" }); got != tc.moved {
					t.Errorf("driftedInPod = %v, want the session store moved %t", drifted, tc.moved)
				}
			})
		}
	}
}

// A daemon restarted on the configuration that built the controller's pod keeps that pod, in
// either of its layouts: the single-role form without a writer audience and the roles-dir form
// with one. Nothing the restarted daemon reads of the stored pod reads as moved — its session
// store, its shape, the secrets broker's enrollment, or the controller's own overlay, whose
// operator mount sits under the home, which the roles-dir form mounts under HomeMountsDir — so its
// launch starts the controller in the pod it finds, enrolled and told its overlay, rather than
// replacing a pod its own configuration built and no round covers.
func TestADaemonKeepsTheControllersPodItsOwnConfigurationBuiltInEitherLayout(t *testing.T) {
	const overlay = podHome + "/operator-roles/controller.yml"
	roles := corev1.Volume{Name: "operator-roles", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-roles"},
	}}}
	for name, store := range map[string]ompsessions.Store{"the single-role form": cloudSQLImpersonatingStore, "the roles-dir form": cloudSQLWriterStore} {
		t.Run(name, func(t *testing.T) {
			configured := func(o *Options) {
				o.SessionStore = store
				o.AgentSecrets = &AgentSecrets{URL: "https://secrets.internal.example", Audience: "agent-secrets", TokenExpiry: time.Hour}
				o.Pod.Volumes = append(o.Pod.Volumes, roles)
				o.Pod.VolumeMounts = append(o.Pod.VolumeMounts, corev1.VolumeMount{Name: roles.Name, MountPath: podHome + "/operator-roles", ReadOnly: true})
				o.RoleConfigFiles = map[claim.Role][]string{claim.RoleController: {overlay}}
			}
			g := newRig(t, nil, withOptions(configured))
			spec := controllerSpec(t)
			loc := g.spawn(spec)
			_, rolesDir := flagValue(containerNamed(t, g.pod(loc.Sandbox.Name).Spec, string(claim.RoleController)).Command, "--roles-dir")
			if want := store.Database.WriterAudience != ""; rolesDir != want {
				t.Fatalf("the controller's launcher runs the roles-dir form: %t, want %t, so this case proves nothing", rolesDir, want)
			}
			r2 := secondRuntime(t, g, configured)()
			g.connectRunning(controllerToken, loc.Sandbox.Generation)
			g.eventually("the controller's launcher to report its child to the restarted daemon", func() bool {
				state, connected := r2.launchers.state(controllerToken, loc.Sandbox.PodUID)
				return connected && state.Child != nil
			})
			if moved := r2.movedInPod(g.pod(loc.Sandbox.Name), claim.RoleController); len(moved) > 0 {
				t.Errorf("the restarted daemon reads %+v as moved in the pod its own configuration built", moved)
			}
			if drifted := r2.shapeDrift(g.sandbox(loc.Sandbox.Name), controllerInGVisor); len(drifted) > 0 {
				t.Errorf("the restarted daemon reads the shape as drifted: %q", drifted)
			}
			spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-controller-g2", controllerSession
			next, err := r2.Resume(g.ctx, &loc, spec)
			if err != nil {
				t.Fatalf("Resume the controller: %v", err)
			}
			if next.Sandbox.PodUID != loc.Sandbox.PodUID {
				t.Fatalf("the controller's launch replaced pod %s with %s, which its own configuration built", loc.Sandbox.PodUID, next.Sandbox.PodUID)
			}
			start := lastStart(t, g, controllerToken)
			if start.Generation != 2 {
				t.Fatalf("the controller's launcher last received generation %d's start command, want generation 2's", start.Generation)
			}
			if !agentSecretsFlags(start, r2) {
				t.Errorf("the start command is %q, want the shim's agent-secrets flags: the pod it runs in carries the broker's enrollment", start.Argv)
			}
			if files := startEnv(start)["PI_CONFIG_FILES"]; !strings.Contains(files, overlay) {
				t.Errorf("the start command's PI_CONFIG_FILES is %q, want %s among it: the pod it runs in mounts that overlay", files, overlay)
			}
		})
	}
}

// An operator's volume may not take the session writer token's name, whether or not the session
// database names a writer audience: a pod of the deployment would carry two volumes of that name
// the day it does.
func TestCheckPodRefusesTheSessionWriterTokensName(t *testing.T) {
	pod := Pod{Volumes: []corev1.Volume{{Name: "session-writer-token", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}
	const want = "runtime.kubernetes.pod.volumes[0].name session-writer-token is a volume Legion puts in every pod"
	for name, store := range map[string]ompsessions.Store{
		"postgres": postgresStore, "cloudsql-iam": cloudSQLImpersonatingStore, "cloudsql-iam with a writer audience": cloudSQLWriterStore,
	} {
		if err := CheckPod(pod, nil, testOptions().Tools, []string{"ENVOY_TOKEN"}, nil, store, false); err == nil || err.Error() != want {
			t.Errorf("under %s, CheckPod = %v, want %q", name, err, want)
		}
	}
}

// An Oh My Pi start names its agent's executable apart from its argv (AgentExecutable): the first
// word of the agent's own argv, the executable the shim's child runs once any start wrapper has
// exec'd it — the image's Oh My Pi by default, the operator's own where Options.Agent names one —
// whether the start runs the agent itself or wraps it in the setup command's `/bin/sh -c …
// legion-start`, whose first word is no agent's. A Claude Code start names none, wrapped or not.
func TestAnOhMyPiStartNamesItsAgentsExecutable(t *testing.T) {
	setup := []string{"make", "setup"}
	for name, tc := range map[string]struct {
		agent, setup []string
		spec         func(*testing.T) runtime.SpawnSpec
		want         string
		wrapped      bool
	}{
		"Oh My Pi":                                   {spec: workerSpec, want: "/opt/omp/bin/omp"},
		"Oh My Pi under a setup command":             {setup: setup, spec: workerSpec, want: "/opt/omp/bin/omp", wrapped: true},
		"the controller's Oh My Pi":                  {spec: controllerSpec, want: "/opt/omp/bin/omp"},
		"the operator's agent":                       {agent: []string{"/opt/agents/bin/omp", "--operator"}, spec: workerSpec, want: "/opt/agents/bin/omp"},
		"the operator's agent under a setup command": {agent: []string{"/opt/agents/bin/omp", "--operator"}, setup: setup, spec: workerSpec, want: "/opt/agents/bin/omp", wrapped: true},
		"Claude Code":                                {spec: claudeWorkerSpec, wrapped: true},
		"Claude Code under a setup command":          {setup: setup, spec: claudeWorkerSpec, wrapped: true},
	} {
		t.Run(name, func(t *testing.T) {
			opts := goldenOptions()
			opts.Agent, opts.Setup = tc.agent, tc.setup
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			start := launcherCommand(launchOf(t, r, tc.spec(t)), r)
			at := slices.Index(start.Argv, "--")
			if at < 0 || at+1 >= len(start.Argv) {
				t.Fatalf("the start command's argv %q carries no agent after --", start.Argv)
			}
			if child := start.Argv[at+1]; (child == "/bin/sh") != tc.wrapped {
				t.Fatalf("the shim's child is %s, want it wrapped %t, so this case proves nothing", child, tc.wrapped)
			}
			if start.AgentExecutable != tc.want {
				t.Errorf("the start command names the agent executable %q, want %q", start.AgentExecutable, tc.want)
			}
		})
	}
}

// The image probe proxies no session, so its pod carries no session writer token under a store
// that names a writer audience: it is the very probe of the same store without one, whose golden
// pins it (TestProbeManifestGoldenAndSchema).
func TestTheImageProbeCarriesNoSessionWriterToken(t *testing.T) {
	at := time.Date(2026, 9, 23, 12, 5, 30, 0, time.UTC)
	probe := ImageProbe{Contract: 3, Resources: probeReservation(), RoleReferences: testRoleReferences}
	writer := storeRuntime(t, cloudSQLWriterStore).probeManifest(probeSandboxName, probe, at)
	plain := storeRuntime(t, cloudSQLImpersonatingStore).probeManifest(probeSandboxName, probe, at)
	checkNoSessionWriter(t, writer.Spec.PodTemplate.Spec)
	if !reflect.DeepEqual(writer, plain) {
		t.Errorf("the probe under a writer audience is %+v, want the probe without one, %+v", writer, plain)
	}
}

// The boot census passes the controller's pod in its roles-dir layout as it passes the single-role
// one: it holds a Sandbox to the one container its labels name and the claim templates its kind
// owns, which the two layouts share.
func TestTheBootCensusPassesTheControllersRolesDirPod(t *testing.T) {
	g := newRig(t, nil, withOptions(func(o *Options) { o.SessionStore = cloudSQLWriterStore }))
	g.spawn(controllerSpec(t))
	g.spawn(rootSpec(t))
	stored := g.sandbox(SandboxName(controllerToken)).Spec.PodTemplate.Spec
	command := containerNamed(t, stored, string(claim.RoleController)).Command
	if _, rolesDir := flagValue(command, "--roles-dir"); !rolesDir {
		t.Fatalf("the controller's stored template runs %q, not the roles-dir form, so this case proves nothing", command)
	}
	if err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject, g.opts.IssueDisks(), slogTo(io.Discard)); err != nil {
		t.Fatalf("census of the controller's roles-dir pod and an issue pod = %v, want none", err)
	}
}

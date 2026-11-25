package sandbox

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testPGDiskOptions are goldenOptions for a deployment that serves the pod's own test database,
// with its disk sized where disk is not "" (runtime.kubernetes.test_postgres_disk).
func testPGDiskOptions(disk string) Options {
	opts := goldenOptions()
	opts.TestPostgres = true
	if disk != "" {
		size := resource.MustParse(disk)
		opts.TestPostgresDisk = &size
	}
	return opts
}

// testPGVolumes are the pod's volumes named testpg: one where the pod runs the test database,
// none where it does not.
func testPGVolumes(pod corev1.PodSpec) []corev1.Volume {
	var found []corev1.Volume
	for _, v := range pod.Volumes {
		if v.Name == testPGVolume {
			found = append(found, v)
		}
	}
	return found
}

// testPGFlags reports whether the roles container's command carries --test-postgres, and
// whether --test-postgres-disk follows it.
func testPGFlags(c corev1.Container) (serves, disk bool) {
	at := slices.Index(c.Command, "--test-postgres")
	return at >= 0, at >= 0 && at+1 < len(c.Command) && c.Command[at+1] == "--test-postgres-disk"
}

// Where the deployment sizes the test database's disk, an issue pod that runs the test database
// keeps its cluster on a generic ephemeral volume of that size, ReadWriteOnce on the class the
// issue's own disk is claimed on — the operator's storage class under gVisor, the microVM block's
// in a microVM — mounted writable where the cluster lives, with no emptyDir of that name, so the
// kubelet's ephemeral-storage limit does not count it. Its launcher is told so, right after
// --test-postgres. The Sandbox carries the claim in fields the CRD declares, so the API server
// keeps every one of them.
func TestPGDiskAnIssuePodKeepsTheTestDatabaseOnADiskOfItsOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		sandbox SandboxConfig
		class   string
	}{
		"gVisor":  {SandboxConfig{}, "gp2"},
		"microVM": {SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}, testMicroVMStorageClass},
	} {
		t.Run(name, func(t *testing.T) {
			opts := testPGDiskOptions("20Gi")
			opts.Sandbox = tc.sandbox
			r, err := configure(opts)
			if err != nil {
				t.Fatal(err)
			}
			l := launchOf(t, r, rootSpec(t))
			if (l.microVM != nil) != (tc.sandbox.Type == SandboxMicroVM) {
				t.Fatalf("the launch runs in a microVM: %t, want %t", l.microVM != nil, tc.sandbox.Type == SandboxMicroVM)
			}
			pod := r.podTemplate(l).Spec
			volumes := testPGVolumes(pod)
			if len(volumes) != 1 {
				t.Fatalf("the pod carries %d volumes named %s (%+v), want one", len(volumes), testPGVolume, volumes)
			}
			cluster := volumes[0].VolumeSource
			if cluster.EmptyDir != nil {
				t.Errorf("the test database's volume is the emptyDir %+v, want a disk of its own", cluster.EmptyDir)
			}
			if cluster.Ephemeral == nil || cluster.Ephemeral.VolumeClaimTemplate == nil {
				t.Fatalf("the test database's volume is %+v, want a generic ephemeral volume", cluster)
			}
			claim := cluster.Ephemeral.VolumeClaimTemplate
			if !reflect.DeepEqual(claim.ObjectMeta, metav1.ObjectMeta{}) {
				t.Errorf("the test database's claim template carries metadata %+v, which the Sandbox CRD prunes", claim.ObjectMeta)
			}
			spec := claim.Spec
			if !slices.Equal(spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) {
				t.Errorf("the test database's disk is claimed %v, want ReadWriteOnce", spec.AccessModes)
			}
			if spec.StorageClassName == nil || *spec.StorageClassName != tc.class {
				t.Errorf("the test database's disk is claimed on class %v, want %s", spec.StorageClassName, tc.class)
			}
			if issue := podDisk(t, pod).Spec.StorageClassName; issue == nil || spec.StorageClassName == nil || *issue != *spec.StorageClassName {
				t.Errorf("the test database's disk is on class %v and the issue's own on %v, want one class", spec.StorageClassName, issue)
			}
			if size, want := spec.Resources.Requests[corev1.ResourceStorage], resource.MustParse("20Gi"); size.Cmp(want) != 0 {
				t.Errorf("the test database's disk requests %s, want %s", size.String(), want.String())
			}
			c := pod.Containers[0]
			mount := mountHolding(c, TestPGDir)
			if mount == nil || mount.Name != testPGVolume || mount.MountPath != TestPGDir || mount.ReadOnly {
				t.Errorf("the roles container mounts %+v at %s, want the writable volume %s", mount, TestPGDir, testPGVolume)
			}
			if serves, disk := testPGFlags(c); !serves || !disk {
				t.Errorf("the roles container runs %q, want --test-postgres followed by --test-postgres-disk", c.Command)
			}
			if found := schemaViolations(manifestOf(t, r, rootSpec(t)), sandboxSchema(t), ""); len(found) > 0 {
				t.Errorf("the manifest has fields the CRD does not declare as sent:\n%s", strings.Join(found, "\n"))
			}
		})
	}
}

// Where the deployment sizes no disk, an issue pod that runs the test database keeps today's
// emptyDir on the node's disk, byte for byte, and its launcher is told of no disk.
func TestPGDiskWithoutTheSettingTheTestDatabaseStaysInItsEmptyDir(t *testing.T) {
	r, err := configure(testPGDiskOptions(""))
	if err != nil {
		t.Fatal(err)
	}
	pod := r.podTemplate(launchOf(t, r, rootSpec(t))).Spec
	want := []corev1.Volume{{Name: testPGVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if got := testPGVolumes(pod); !reflect.DeepEqual(got, want) {
		t.Errorf("the test database's volumes are %+v, want exactly %+v", got, want)
	}
	if serves, disk := testPGFlags(pod.Containers[0]); !serves || disk || slices.Contains(pod.Containers[0].Command, "--test-postgres-disk") {
		t.Errorf("the roles container runs %q, want --test-postgres and no --test-postgres-disk", pod.Containers[0].Command)
	}
}

// A pod that runs no test database has neither the volume nor the flag, whatever the disk is set
// to: the controller's pod of a deployment that sizes the disk, and an issue pod of a runtime that
// serves no test database.
func TestPGDiskAPodThatRunsNoTestDatabaseHasNoDisk(t *testing.T) {
	sized, err := configure(testPGDiskOptions("20Gi"))
	if err != nil {
		t.Fatal(err)
	}
	noDatabase := testPGDiskOptions("20Gi")
	noDatabase.TestPostgres = false
	off, err := configure(noDatabase)
	if err != nil {
		t.Fatal(err)
	}
	for name, pod := range map[string]corev1.PodSpec{
		"the controller's pod":                  sized.podTemplate(launchOf(t, sized, controllerSpec(t))).Spec,
		"an issue pod serving no test database": off.podTemplate(launchOf(t, off, rootSpec(t))).Spec,
	} {
		if volumes := testPGVolumes(pod); len(volumes) != 0 {
			t.Errorf("%s carries %+v, want no %s volume", name, volumes, testPGVolume)
		}
		for _, c := range pod.Containers {
			if slices.Contains(c.Command, "--test-postgres") || slices.Contains(c.Command, "--test-postgres-disk") {
				t.Errorf("%s's container %s runs %q, want neither test database flag", name, c.Name, c.Command)
			}
			if mount := mountHolding(c, TestPGDir); mount != nil && mount.Name == testPGVolume {
				t.Errorf("%s's container %s mounts %+v", name, c.Name, mount)
			}
		}
	}
}

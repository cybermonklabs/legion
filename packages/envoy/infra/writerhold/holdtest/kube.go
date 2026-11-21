package holdtest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The clustered hub's names in the Kubernetes API, as the deployment names them and writerhold's
// KubeHub reads them (writerhold.DefaultNamespace and its neighbours, restated because this package
// imports nothing of writerhold).
const (
	Namespace   = "envoy"
	StatefulSet = "envoy-nats"
	ConfigMap   = "envoy-nats-config"
	// ClaimTemplate is the StatefulSet's one volume claim template, the JetStream store's: pod
	// envoy-nats-<i>'s claim is envoy-nats-js-envoy-nats-<i>, as the nats chart names it.
	ClaimTemplate = "envoy-nats-js"
	// Container is the nats-server container of each pod.
	Container = "nats"
	// Revision is the StatefulSet's status.updateRevision, and every pod's controller-revision-hash.
	Revision = StatefulSet + "-5d8f7b9c4f"
	// Digest is the image digest the StatefulSet pins and each pod's container runs.
	Digest = "sha256:4b1f7c2e9d0a8b6c5e3f2a1d0c9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b"
	// MonitorPort and ClientPort are nats-server's monitoring and client ports, which Forward maps
	// to a member's own.
	MonitorPort = 8222
	ClientPort  = 4222
)

// Image is the image reference the StatefulSet pins: the version of the nats-server this test
// binary links, at Digest.
var Image = "nats:" + natsserver.VERSION + "@" + Digest

// Kube is the clustered hub's Kubernetes API over a testnats cluster: a fake clientset holding
// StatefulSet envoy-nats with one replica per member, pod envoy-nats-<i> for member i (Ready, its
// controller-revision-hash the StatefulSet's update revision, its nats container running Image),
// each pod's JetStream claim bound to a volume of its own, and a Forwarder that reaches member i's
// monitoring and client ports for pod envoy-nats-<i>.
//
// Its eviction (policy/v1, the pods/eviction subresource) does what Kubernetes does under the
// hub's disruption budget: while the budget is spent (SpendBudget) it refuses with 429; else it
// stops member i gracefully (testnats Server.Stop, nats-server's Shutdown), records the exit code
// (SetExitCode, 0 unless set) as the old pod's container lastState.terminated, deletes the pod,
// starts member i again on its own store, and creates pod envoy-nats-<i> anew with a new UID, as
// the StatefulSet controller does. The eviction stops and starts the member before it returns, so
// it is called from the test's own goroutine (testnats' Start fails the test there).
type Kube struct {
	Client  *fake.Clientset
	Cluster *testnats.Cluster

	t  testing.TB
	mu sync.Mutex
	// generation counts each pod's incarnations, its UID's suffix.
	generation []int
	spent      bool
	refused    int
	keep       map[int]bool
	exitCode   int32
	down       func(i int)
	evicted    []string
}

// NewKube is the Kube of cluster, whose fake objects and forwards last as long as the test.
func NewKube(t testing.TB, cluster *testnats.Cluster) *Kube {
	t.Helper()
	k := &Kube{Cluster: cluster, t: t, generation: make([]int, len(cluster.Servers)), keep: map[int]bool{}}
	replicas := int32(len(cluster.Servers))
	objects := []runtime.Object{&appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: StatefulSet, Namespace: Namespace, UID: types.UID(StatefulSet + "-uid")},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: StatefulSet,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "nats"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "nats"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: Container, Image: Image}}},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: ClaimTemplate},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
				},
			}},
		},
		Status: appsv1.StatefulSetStatus{
			Replicas: replicas, ReadyReplicas: replicas, CurrentReplicas: replicas, UpdatedReplicas: replicas,
			CurrentRevision: Revision, UpdateRevision: Revision,
		},
	}}
	for i := range cluster.Servers {
		objects = append(objects, k.pod(i))
		claim, volume := Claim(i), "pvc-"+Pod(i)
		objects = append(objects,
			&corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: Namespace, UID: types.UID(claim + "-uid")},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					VolumeName:  volume,
				},
				Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
			},
			&corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: volume},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "pd.csi.storage.gke.io",
						VolumeHandle: "projects/holdtest/zones/us-central1-a/disks/" + volume,
					}},
					ClaimRef: &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: Namespace, Name: claim},
				},
				Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
			})
	}
	k.Client = fake.NewClientset(objects...)
	k.Client.PrependReactor("create", "pods", k.evict)
	return k
}

// Pod is member i's pod, envoy-nats-<i>.
func Pod(i int) string { return fmt.Sprintf("%s-%d", StatefulSet, i) }

// Claim is pod envoy-nats-<i>'s JetStream claim.
func Claim(i int) string { return ClaimTemplate + "-" + Pod(i) }

// member is the index of the member pod runs, or -1 for a pod the hub does not have.
func (k *Kube) member(pod string) int {
	for i := range k.Cluster.Servers {
		if Pod(i) == pod {
			return i
		}
	}
	return -1
}

// pod is pod envoy-nats-<i> of its current incarnation: Ready, running Image at Digest.
func (k *Kube) pod(i int) *corev1.Pod {
	started := metav1.NewTime(time.Now())
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: Pod(i), Namespace: Namespace,
			UID: types.UID(fmt.Sprintf("%s-uid-%d", Pod(i), k.generation[i])),
			Labels: map[string]string{
				"app.kubernetes.io/name":             "nats",
				"controller-revision-hash":           Revision,
				"statefulset.kubernetes.io/pod-name": Pod(i),
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: StatefulSet, UID: types.UID(StatefulSet + "-uid")}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: Container, Image: Image}},
			Volumes: []corev1.Volume{{Name: ClaimTemplate, VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: Claim(i)},
			}}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: Container, Image: Image, ImageID: "docker.io/library/nats@" + Digest, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
			}},
		},
	}
}

// SpendBudget makes the hub's disruption budget spent, so every eviction is refused with 429 (as
// Kubernetes refuses one that would violate the budget), or not.
func (k *Kube) SpendBudget(spent bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.spent = spent
}

// Refused is how many evictions the spent budget refused.
func (k *Kube) Refused() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.refused
}

// KeepProcess makes the evictions of member i's pod delete and create the pod but leave the
// member's nats-server running, as a server that never restarted would look: the new pod answers
// with the same server id.
func (k *Kube) KeepProcess(i int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keep[i] = true
}

// SetExitCode is the exit code each later eviction records for the stopped container: 0, a
// graceful stop, unless set; 137 is a container killed after its grace period.
func (k *Kube) SetExitCode(code int32) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.exitCode = code
}

// WhileDown is called by each later eviction with the member's index once the member has stopped
// and before it starts again, as a pod is gone for a while before the StatefulSet's new one runs:
// a test waits there for what the cluster does without that member. It runs on the evicting
// goroutine, inside the fake API's lock, so it must not call Client.
func (k *Kube) WhileDown(down func(i int)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.down = down
}

// Evicted are the pods the hub evicted, in order.
func (k *Kube) Evicted() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.evicted...)
}

var podsResource = corev1.SchemeGroupVersion.WithResource("pods")

// evict is the eviction subresource's reactor (Kube's comment). It runs inside the fake's lock, so
// it changes the objects through the tracker alone.
func (k *Kube) evict(action k8stesting.Action) (bool, runtime.Object, error) {
	if action.GetSubresource() != "eviction" {
		return false, nil, nil
	}
	create, ok := action.(k8stesting.CreateAction)
	if !ok {
		return true, nil, apierrors.NewBadRequest("an eviction is a create")
	}
	eviction, ok := create.GetObject().(*policyv1.Eviction)
	if !ok {
		return true, nil, apierrors.NewBadRequest(fmt.Sprintf("an eviction's body is a policy/v1 Eviction, not %T", create.GetObject()))
	}
	namespace, name := action.GetNamespace(), eviction.Name
	i := k.member(name)
	if namespace != Namespace || i < 0 {
		return true, nil, apierrors.NewNotFound(podsResource.GroupResource(), name)
	}
	tracker := k.Client.Tracker()
	object, err := tracker.Get(podsResource, namespace, name)
	if err != nil {
		return true, nil, err
	}
	old := object.(*corev1.Pod).DeepCopy()
	if eviction.DeleteOptions != nil && eviction.DeleteOptions.Preconditions != nil && eviction.DeleteOptions.Preconditions.UID != nil &&
		*eviction.DeleteOptions.Preconditions.UID != old.UID {
		return true, nil, apierrors.NewConflict(podsResource.GroupResource(), name,
			fmt.Errorf("the UID in the precondition (%s) does not match the UID in record (%s)", *eviction.DeleteOptions.Preconditions.UID, old.UID))
	}
	k.mu.Lock()
	if k.spent {
		k.refused++
		k.mu.Unlock()
		return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 10)
	}
	keep, code, down := k.keep[i], k.exitCode, k.down
	k.evicted = append(k.evicted, name)
	k.mu.Unlock()

	if !keep {
		k.Cluster.Servers[i].Stop(k.t)
	}
	if down != nil {
		down(i)
	}
	reason := "Completed"
	if code != 0 {
		reason = "Error"
	}
	finished := metav1.NewTime(time.Now())
	old.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	for c := range old.Status.ContainerStatuses {
		status := &old.Status.ContainerStatuses[c]
		status.Ready = false
		status.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, FinishedAt: finished,
		}}
	}
	if err := tracker.Update(podsResource, old, namespace); err != nil {
		return true, nil, err
	}
	if err := tracker.Delete(podsResource, namespace, name); err != nil {
		return true, nil, err
	}
	if !keep {
		k.Cluster.Servers[i].Start(k.t)
	}
	k.mu.Lock()
	k.generation[i]++
	pod := k.pod(i)
	k.mu.Unlock()
	if err := tracker.Create(podsResource, pod, namespace); err != nil {
		return true, nil, err
	}
	return true, nil, nil
}

// Forward is the hub's port-forward (writerhold's Forwarder): port 8222 of pod envoy-nats-<i>
// reaches member i's monitoring listener, read at each call because a restart moves it, and port
// 4222 member i's own client URL alone. A stopped member's monitoring port does not forward.
func (k *Kube) Forward(_ context.Context, namespace, pod string, port int) (string, func(), error) {
	i := k.member(pod)
	if namespace != Namespace || i < 0 {
		return "", nil, fmt.Errorf("no pod %s/%s", namespace, pod)
	}
	server := k.Cluster.Servers[i]
	switch port {
	case MonitorPort:
		monitor := server.MonitorURL()
		if monitor == "" {
			return "", nil, fmt.Errorf("pod %s/%s's nats-server is not running, so its port %d does not forward", namespace, pod, port)
		}
		return strings.TrimPrefix(monitor, "http://"), func() {}, nil
	case ClientPort:
		return strings.TrimPrefix(server.URL(), "nats://"), func() {}, nil
	}
	return "", nil, fmt.Errorf("pod %s/%s has no port %d", namespace, pod, port)
}

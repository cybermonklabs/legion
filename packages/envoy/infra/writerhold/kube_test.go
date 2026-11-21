package writerhold

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The declared servers are the StatefulSet's pods by the ordinals its controller gives them: from
// spec.ordinals.start, replicas of them. A StatefulSet whose ordinals start at 3 runs envoy-nats-3
// to envoy-nats-5, and a reader that counted from 0 would read pods that do not exist and miss
// every one that does. The render reader and KubeHub.Pods read the same names. It fails when
// either counts from 0 whatever the StatefulSet says.
func TestTheDeclaredPodsFollowTheOrdinalsStart(t *testing.T) {
	replicas := int32(3)
	for _, row := range []struct {
		name     string
		ordinals *appsv1.StatefulSetOrdinals
		want     []string
	}{
		{"no ordinals", nil, []string{"envoy-nats-0", "envoy-nats-1", "envoy-nats-2"}},
		{"ordinals from 3", &appsv1.StatefulSetOrdinals{Start: 3}, []string{"envoy-nats-3", "envoy-nats-4", "envoy-nats-5"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			set := &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: DefaultStatefulSet},
				Spec:       appsv1.StatefulSetSpec{Replicas: &replicas, Ordinals: row.ordinals},
			}
			hub := KubeHub{Client: fake.NewClientset(set), Namespace: DefaultNamespace, StatefulSet: DefaultStatefulSet}
			pods, err := hub.Pods(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(pods) != len(row.want) {
				t.Fatalf("KubeHub.Pods = %v, want %v", pods, row.want)
			}
			for i := range pods {
				if pods[i] != row.want[i] {
					t.Fatalf("KubeHub.Pods = %v, want %v", pods, row.want)
				}
			}
			named, err := statefulSetPods(set)
			if err != nil {
				t.Fatal(err)
			}
			for i := range named {
				if named[i] != pods[i] {
					t.Fatalf("the render reader's pods %v differ from KubeHub.Pods %v", named, pods)
				}
			}
		})
	}
}

package writerhold

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// retiredStores is the ConfigMap the deployment renders for the projects retired after the
// fresh-name cutover, holding data: namespace envoy-operator, name envoy-operator-retired-stores,
// written out here so the row pins the name the deployment renders.
func retiredStores(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "envoy-operator", Name: "envoy-operator-retired-stores"}, Data: data}
}

// listedSources are listed, each as its name, whether it is a retired project's entry, and its
// annotations in key order, so a row compares them whole.
func listedSources(listed []DaemonDeployment) []string {
	out := make([]string, len(listed))
	for i, d := range listed {
		var annotations []string
		for _, key := range slices.Sorted(maps.Keys(d.Annotations)) {
			annotations = append(annotations, key+"="+d.Annotations[key])
		}
		source := "Deployment "
		if d.Retired {
			source = "retired "
		}
		out[i] = source + d.Name + " " + strings.Join(annotations, " ")
	}
	return out
}

// KubeDaemonDeployments, the one reader of the daemon stores' addresses, lists the Deployments of
// namespace legion and then each retired project's entry of ConfigMap
// envoy-operator/envoy-operator-retired-stores, by its project key, its two keys read as the
// annotations a Deployment carries: an entry with one key missing is listed without that
// annotation, for the readers of the daemon stores to refuse by name. The deployment renders that
// ConfigMap always, with `data: {}` when no project is retired, which holds no retired project; a
// ConfigMap that does not exist holds none either, and the Deployments are still listed. A
// ConfigMap read the API server refuses, and a key of neither form (<project>.database,
// <project>.instance), are the reader's error, naming the ConfigMap. Controls: a reader that lists
// the Deployments alone and never reads the ConfigMap, as the one before did, fails each part but
// the ones with no entry; a reader that refuses a ConfigMap with no entry fails the empty one.
func TestKubeDaemonDeploymentsReadsTheRetiredStores(t *testing.T) {
	ctx := context.Background()
	live := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "legion-daemon-live", Namespace: DaemonNamespace,
		Annotations: map[string]string{DaemonProjectAnnotation: "LIVE", DaemonStoreDatabaseAnnotation: "daemon_live", DaemonStoreInstanceAnnotation: "proj:region:daemons"}}}
	deployment := "Deployment legion-daemon-live " + DaemonProjectAnnotation + "=LIVE " + DaemonStoreDatabaseAnnotation + "=daemon_live " +
		DaemonStoreInstanceAnnotation + "=proj:region:daemons"

	client := fake.NewClientset(live, retiredStores(map[string]string{
		"RETIRED.database": "daemon_retired", "RETIRED.instance": "proj:region:daemons", "HALF.instance": "proj:region:daemons",
	}))
	listed, err := KubeDaemonDeployments(client)(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{
		deployment,
		"retired HALF " + DaemonProjectAnnotation + "=HALF " + DaemonStoreInstanceAnnotation + "=proj:region:daemons",
		"retired RETIRED " + DaemonProjectAnnotation + "=RETIRED " + DaemonStoreDatabaseAnnotation + "=daemon_retired " +
			DaemonStoreInstanceAnnotation + "=proj:region:daemons",
	}, listedSources(listed))

	listed, err = KubeDaemonDeployments(fake.NewClientset(live))(ctx)
	require.NoError(t, err, "a missing ConfigMap is no error")
	require.Equal(t, []string{deployment}, listedSources(listed), "a missing ConfigMap holds no retired project")

	for _, data := range []map[string]string{{}, nil} {
		listed, err = KubeDaemonDeployments(fake.NewClientset(live, retiredStores(data)))(ctx)
		require.NoError(t, err, "a ConfigMap with data %v", data)
		require.Equal(t, []string{deployment}, listedSources(listed), "a ConfigMap with data %v holds no retired project", data)
	}

	_, err = KubeDaemonDeployments(fake.NewClientset(live, retiredStores(map[string]string{"RETIRED.database": "daemon_retired", "RETIRED.url": "x"})))(ctx)
	require.ErrorContains(t, err, `"RETIRED.url"`)
	require.ErrorContains(t, err, "envoy-operator/envoy-operator-retired-stores")

	refused := fake.NewClientset(live, retiredStores(map[string]string{"RETIRED.database": "daemon_retired", "RETIRED.instance": "proj:region:daemons"}))
	refused.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(`configmaps "envoy-operator-retired-stores" is forbidden`)
	})
	_, err = KubeDaemonDeployments(refused)(ctx)
	require.ErrorContains(t, err, `configmaps "envoy-operator-retired-stores" is forbidden`)
	require.ErrorContains(t, err, "ConfigMap envoy-operator/envoy-operator-retired-stores")
}

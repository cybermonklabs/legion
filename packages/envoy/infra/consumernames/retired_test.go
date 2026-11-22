package consumernames

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// The rows of a Legion project retired after the fresh-name cutover: it runs no Deployment, its
// cutover row daemon/<P> stays, and the deployment renders its store's address in ConfigMap
// envoy-operator/envoy-operator-retired-stores, as data keys <P>.database and <P>.instance. The
// rows write the ConfigMap's namespace, name and keys out, so they pin what the deployment renders.

// retiredSource is how the readers of the daemon stores name the retired projects' ConfigMap.
const retiredSource = "ConfigMap envoy-operator/envoy-operator-retired-stores"

// retiredStores is the retired projects' ConfigMap holding data.
func retiredStores(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "envoy-operator", Name: "envoy-operator-retired-stores"}, Data: data}
}

// retiredEntry is project's entry of the retired projects' ConfigMap: its store's database and the
// instance that holds it, each key left out when its value is "".
func retiredEntry(project, database, instance string) map[string]string {
	entry := map[string]string{}
	if database != "" {
		entry[project+".database"] = database
	}
	if instance != "" {
		entry[project+".instance"] = instance
	}
	return entry
}

// deploymentObject is d rendered as the Deployment in namespace writerhold.DaemonNamespace that a
// fake Kubernetes client lists.
func deploymentObject(d writerhold.DaemonDeployment) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: writerhold.DaemonNamespace, Annotations: d.Annotations}}
}

// retire renders the retired projects' ConfigMap in the stack's Kubernetes with data, in place of
// the one there, where every reader of the daemon stores reads it with the hold's own client.
func (h *stack) retire(data map[string]string) {
	h.t.Helper()
	configMaps := h.s.Kube.Client.CoreV1().ConfigMaps("envoy-operator")
	_, err := configMaps.Update(h.t.Context(), retiredStores(data), metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(h.t.Context(), retiredStores(data), metav1.CreateOptions{})
	}
	require.NoError(h.t, err)
}

// The census's rule for the daemon stores, through the one reader of their addresses
// (writerhold.KubeDaemonDeployments over a Kubernetes client), reads each store at the address
// exactly one source names: a project with no Deployment and an entry in the retired projects'
// ConfigMap is read there, and its store, whose database the server answers 3D000 for, is gone.
// Every other case is unreconciled, naming the project and why, and nothing is gone: a project a
// Deployment and an entry both name, naming both sources; a project in neither, beside another
// project's entry or with no ConfigMap at all, which is no error; an entry missing its database
// key or its instance key, naming the key; and an entry whose address a live project's Deployment
// names too, naming both sources and both projects. A read of the ConfigMap the API server refuses
// fails the whole call, naming the ConfigMap, so the census is UNKNOWN. Control (a red run,
// by -overlay): the reader before it read the retired projects' ConfigMap leaves the retired
// project unreconciled ("0 Deployments of namespace legion name project RETIRED"), reads BOTH at
// its Deployment and calls it gone, and answers the refused read with no error.
func TestTheCensusRuleReadsEachStoreAtOneSource(t *testing.T) {
	ctx := t.Context()
	conn, _ := censusHeldOperator(t, "stream-2")
	_, server := censusStore(t, "SERVER")
	dropped := censusDroppedDatabase()
	for _, row := range []struct {
		name, project string
		objects       []runtime.Object
		// reason is what the project's unreconciled reason names, nil for a store read gone.
		reason []string
	}{
		{"a retired project's entry alone", "RETIRED",
			[]runtime.Object{retiredStores(retiredEntry("RETIRED", dropped, censusInstance))}, nil},
		{"a Deployment and an entry both", "BOTH",
			[]runtime.Object{deploymentObject(censusDeployment("BOTH", dropped, censusInstance)), retiredStores(retiredEntry("BOTH", dropped, censusInstance))},
			[]string{"project BOTH", "Deployment legion/legion-daemon-both", retiredSource + " names it", "not one source"}},
		{"neither, beside another project's entry", "NEITHER",
			[]runtime.Object{retiredStores(retiredEntry("OTHER", dropped, censusInstance))},
			[]string{"0 Deployments of namespace legion name project NEITHER", retiredSource + " does not name it", "NEITHER.database"}},
		{"neither, with no ConfigMap", "NEITHER", nil,
			[]string{"0 Deployments of namespace legion name project NEITHER", retiredSource + " does not name it"}},
		{"an entry with no database key", "HALF",
			[]runtime.Object{retiredStores(retiredEntry("HALF", "", censusInstance))}, []string{retiredSource + " has no key HALF.database"}},
		{"an entry with no instance key", "HALF",
			[]runtime.Object{retiredStores(retiredEntry("HALF", dropped, ""))}, []string{retiredSource + " has no key HALF.instance"}},
		{"an entry at an address a Deployment names too", "RETIRED",
			[]runtime.Object{deploymentObject(censusDeployment("LIVE", dropped, censusInstance)), retiredStores(retiredEntry("RETIRED", dropped, censusInstance))},
			[]string{retiredSource + " (project RETIRED)", "Deployment legion/legion-daemon-live (project LIVE)",
				fmt.Sprintf("each name database %q on instance %q", dropped, censusInstance)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			entry := censusStoreFileFor(t, row.project, server, dropped, censusInstance)
			families := []string{daemonFamily(row.project)}
			result, err := ReconcileDaemonStreamIDs(ctx, conn, writerhold.KubeDaemonDeployments(fake.NewClientset(row.objects...)), families, []string{entry})
			require.NoError(t, err)
			require.NoError(t, result.Covers(families))
			if row.reason == nil {
				require.Empty(t, result.Unreconciled, "%+v", result.Unreconciled)
				require.Len(t, result.Gone, 1, "%+v", result)
				require.Equal(t, writerhold.GoneStore{Project: row.project, Database: dropped, Instance: censusInstance, StatedInstance: censusInstance,
					Answer: result.Gone[0].Answer}, result.Gone[0])
				require.Contains(t, result.Gone[0].Answer, "SQLSTATE 3D000")
				return
			}
			require.Empty(t, result.Gone, "a store no one source names was called gone")
			require.Len(t, result.Unreconciled, 1, "%+v", result)
			require.Equal(t, row.project, result.Unreconciled[0].Project)
			for _, want := range row.reason {
				require.Contains(t, result.Unreconciled[0].Reason, want)
			}
		})
	}

	t.Run("a read of the ConfigMap refused", func(t *testing.T) {
		client := fake.NewClientset(retiredStores(retiredEntry("RETIRED", dropped, censusInstance)))
		client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New(`configmaps "envoy-operator-retired-stores" is forbidden`)
		})
		entry := censusStoreFileFor(t, "RETIRED", server, dropped, censusInstance)
		_, err := ReconcileDaemonStreamIDs(ctx, conn, writerhold.KubeDaemonDeployments(client), []string{daemonFamily("RETIRED")}, []string{entry})
		require.ErrorContains(t, err, retiredSource+`: configmaps "envoy-operator-retired-stores" is forbidden`)
	})
}

// A project retired after the cutover, its cutover row present, no Deployment, its entry in the
// retired projects' ConfigMap and its database dropped (the server answers 3D000 at that address),
// through the commands every reader of its store runs, each listing the addresses with the hold's
// own Kubernetes client (writerhold.KubeDaemonDeployments). The census through its word reads the
// store gone at its entry's address, both of its readers alike: it leaves the retired daemon's
// consumers, naming each LEFT (the reader of the fresh names), prints `GONE daemon/RETIRED: ...`
// (the writer of the stream ids, the call hub-restore's abort makes too), and completes, its done
// entry naming gone.daemon/RETIRED. Then cleanup --gone, in a consumer-cleanup hold, reads the
// store gone at the same address and removes those consumers. Before that, a census whose read of
// the ConfigMap the API server refuses is UNKNOWN, naming the ConfigMap, removes nothing, and its
// stop records only this hold's barrier key. Control (a red run, by -overlay): the reader before
// it read the retired projects' ConfigMap leaves daemon/RETIRED unreconciled ("0 Deployments of
// namespace legion name project RETIRED"), so every census is UNKNOWN and cleanup --gone refuses.
func TestTheCensusReadsARetiredProjectsStoreGoneAtItsEntry(t *testing.T) {
	h := newStack(t)
	ctx := t.Context()
	census := h.heldHold(holdrecord.WorkflowStreamsCensus)
	nc, js := h.operator()
	censusNotificationStream(t, js)
	_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
	require.NoError(t, err)
	const wake, dispatch = "legion-go-RETIRED-wake", "legion-go-RETIRED-dispatch"
	retired := []string{selection.FreshName(wake), dispatch}
	for name, logical := range map[string]string{retired[0]: wake, dispatch: dispatch} {
		_, err := writeConsumer(ctx, nc, notifications, jetstream.ConsumerConfig{Durable: name, FilterSubject: bus.LegionWakeTopic,
			AckPolicy: jetstream.AckExplicitPolicy, Metadata: map[string]string{selection.LogicalMetadataKey: logical}}, actionCreateConsumer)
		require.NoError(t, err, "create %s", name)
	}
	for _, family := range []string{familySession, daemonFamily("RETIRED")} {
		_, err := h.records().PutCutover(ctx, family, selection.CutoverDone, census)
		require.NoError(t, err)
	}
	_, server := censusStore(t, "SERVER")
	dropped := censusDroppedDatabase()
	h.retire(retiredEntry("RETIRED", dropped, censusInstance))
	entry := censusStoreFileFor(t, "RETIRED", server, dropped, censusInstance)
	flags := []string{"--family", familySession, "--family", daemonFamily("RETIRED"), "--daemon-consumer", wake + "=" + bus.LegionWakeTopic,
		"--daemon-database", entry}
	consumers := func() []string {
		nc, _ := h.operator()
		return censusConsumers(t, nc)
	}

	var refuse atomic.Bool
	refuse.Store(true)
	h.s.Kube.Client.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if refuse.Load() && action.(k8stesting.GetAction).GetName() == "envoy-operator-retired-stores" {
			return true, nil, errors.New(`configmaps "envoy-operator-retired-stores" is forbidden`)
		}
		return false, nil, nil
	})
	code, stdout, stderr := h.censusCommand(census, flags...)
	require.Equal(t, 2, code, "a census whose read of the ConfigMap is refused: %s%s", stdout, stderr)
	require.Contains(t, stderr, "UNKNOWN")
	require.Contains(t, stderr, retiredSource+`: configmaps "envoy-operator-retired-stores" is forbidden`)
	require.NotContains(t, stdout, "GONE daemon/")
	require.Equal(t, barrierLeft, h.lastEntry(census, "").Outputs["left"], "the stop records the barrier key the census put, and nothing more")
	require.Subset(t, consumers(), retired, "an UNKNOWN census removed a consumer")
	refuse.Store(false)

	code, stdout, stderr = h.censusCommand(census, flags...)
	require.Equal(t, 0, code, "the census beside a retired project's gone store: %s%s", stdout, stderr)
	why := "left, store gone: daemon/RETIRED's database " + dropped + " on instance " + censusInstance + " does not exist"
	for _, name := range retired {
		require.Contains(t, stdout, "LEFT "+notifications+" consumer "+name+": "+why+"\n")
	}
	gone := "database " + dropped + " on instance " + censusInstance + " (the operator's file states " + censusInstance + "): "
	require.Contains(t, stdout, "GONE daemon/RETIRED: "+gone)
	require.True(t, slices.Contains(strings.Split(strings.TrimSpace(stdout), "\n"), "CLEAN"), stdout)
	done := h.lastEntry(census, "")
	require.Equal(t, writerhold.CommandStreamsCensus+" done", done.Command+" "+done.State)
	require.True(t, strings.HasPrefix(done.Outputs["gone.daemon/RETIRED"], gone), "%q", done.Outputs["gone.daemon/RETIRED"])
	require.Contains(t, done.Outputs["gone.daemon/RETIRED"], "SQLSTATE 3D000")
	require.Subset(t, consumers(), retired, "the census removed a retired daemon's consumer")
	require.NoError(t, h.release(census))

	cleanup := h.heldHold(holdrecord.WorkflowConsumerCleanup)
	code, stdout, stderr = h.run(actionCleanup, "--hold", cleanup, "--gone", "RETIRED", "--daemon-database", entry)
	require.Equal(t, 0, code, "cleanup --gone RETIRED: %s%s", stdout, stderr)
	for _, name := range retired {
		require.Contains(t, stdout, "cleanup --gone daemon/RETIRED: removed "+notifications+" consumer "+name+": store gone: "+gone)
	}
	left := consumers()
	for _, name := range retired {
		require.NotContains(t, left, name, "cleanup --gone left the retired daemon's %s", name)
	}
	done = h.lastEntry(cleanup, "")
	require.Equal(t, "consumer-names cleanup --gone done", done.Command+" "+done.State)
	require.True(t, strings.HasPrefix(done.Outputs["gone.daemon/RETIRED"], gone), "%q", done.Outputs["gone.daemon/RETIRED"])
	code, stdout, stderr = h.run(actionCleanup, "--hold", cleanup, "--gone", "RETIRED", "--daemon-database", entry, "--final")
	require.Equal(t, 0, code, "cleanup --gone RETIRED --final: %s%s", stdout, stderr)
	require.NoError(t, h.release(cleanup))
}

// enroll refuses a store whose address, a database on an instance, a retired project's entry in
// the retired projects' ConfigMap names too, as it refuses one another Deployment names: enroll
// writes a stop entry naming the project's Deployment and the ConfigMap, with both projects,
// recording the run unchanged, writes neither the store's marker nor the broker's cutover record,
// and the hold is released. Control: with the entry naming another database, the same enroll is
// done and marks the store; and the reader before it read the retired projects' ConfigMap (a red
// run, by -overlay) lets the first enroll through.
func TestEnrollRefusesAStoreAddressARetiredProjectNames(t *testing.T) {
	h := newStack(t)
	ctx := context.Background()
	const project, retired = "ENRLIVE", "ENRRETIRED"
	db := h.database(daemonFamily(project), daemonMigrations...)
	h.replaceNotificationStream()
	h.enrollWindow()
	database := db.Config().ConnConfig.Database
	h.censusDeploy(censusDeploymentOf(project, db))
	h.retire(retiredEntry(retired, database, censusInstance))
	h.enrollRequireStopped(project, enrollStore(t, project, db), daemonFamily(project)+"'s store is unreconciled, so enroll marks nothing",
		"Deployment legion/legion-daemon-enrlive (project ENRLIVE)", retiredSource+" (project ENRRETIRED)",
		fmt.Sprintf("each name database %q on instance %q", database, censusInstance))

	// Control: the retired project's entry names another database.
	h.retire(retiredEntry(retired, "daemon_enrretired", censusInstance))
	id := h.heldHold(holdrecord.WorkflowEnroll)
	code, stdout, stderr := h.enrollRun(id, project)
	require.Equal(t, 0, code, "control: enroll beside an entry at another address: %s%s", stdout, stderr)
	state, err := enrollStore(t, project, db).State(ctx)
	require.NoError(t, err)
	require.Equal(t, id, state.Hold, "control: the enroll marked the store")
	require.NoError(t, h.release(id))
}

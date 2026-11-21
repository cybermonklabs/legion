package busprojects_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects"
	"github.com/sjawhar/envoy/internal/dispatch/busprojects/busprojectstest"
	"github.com/sjawhar/envoy/natsgrant"
)

// The three-replica stand-in: one listener workload whose instances name resolves to three
// addresses, each answered by the transport the test gives the admission.
const (
	workload        = "envoy-listener"
	workloadHost    = "envoy-listener.example.invalid"
	workloadName    = "envoy-listener-instances.example.invalid"
	workloadSource  = "https://broker.example.invalid"
	replicaHealthy1 = "10.0.0.1"
	replicaBroken   = "10.0.0.2"
	replicaHealthy3 = "10.0.0.3"
)

// replicaAnswer is how one stand-in replica answers the privacy-readiness read: a status and body,
// or no answer at all.
type replicaAnswer struct {
	status   int
	body     string
	noAnswer bool
}

func answered(instance string) replicaAnswer {
	return replicaAnswer{status: http.StatusOK, body: `{"instance":"` + instance + `","required":true,"source":"` + workloadSource + `","read":"answered"}`}
}

// threeReplicas is the stand-in workload: its inventory entry, and a client whose transport
// answers each replica's address as answers say, and a request that is not the replica's
// privacy-readiness read with the workload's Host 404.
func threeReplicas(answers map[string]replicaAnswer) ([]busprojects.ListenerTarget, *http.Client) {
	target := busprojects.ListenerTarget{Name: workload, Source: workloadSource, Host: workloadHost, Instances: workloadName + ":9020"}
	client := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		answer, ok := answers[r.URL.Hostname()]
		switch {
		case !ok || answer.noAnswer:
			return nil, errors.New("dial tcp " + r.URL.Host + ": connect: connection refused")
		case r.Method != http.MethodGet || r.URL.Path != busprojects.ReadinessPath || r.URL.Port() != "9020" || r.Host != workloadHost:
			return respond(r, http.StatusNotFound, "not the replica's privacy-readiness read"), nil
		}
		return respond(r, answer.status, answer.body), nil
	})}
	return []busprojects.ListenerTarget{target}, client
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}
}

// resolveWorkload is the stand-in resolver: workloadName's three addresses, given out of order,
// and no address for any other name.
func resolveWorkload(_ context.Context, host string) ([]string, error) {
	if host == workloadName {
		return []string{replicaHealthy3, replicaHealthy1, replicaBroken}, nil
	}
	return nil, nil
}

// proofToken is Dispatch's hub-admission proof token.
func proofToken() string {
	return busprojectstest.ProofToken(busprojectstest.DispatchAccount, natsgrant.HubAdmissionAudience)
}

// admissionOver is the admission over one proof client per hub, as cmd/dispatch builds it, with
// the stand-in resolver. A tagged hub's client is awaited until it has connected, so the hub
// admits Dispatch before the test proves.
func admissionOver(t *testing.T, hubs ...*busprojectstest.Hub) (*busprojects.HubAdmission, []*bus.Client) {
	t.Helper()
	clients := make([]*bus.Client, len(hubs))
	for i, h := range hubs {
		clients[i] = busprojectstest.ProofClient(t, h.URL, proofToken())
	}
	a := busprojects.NewHubAdmission(clients)
	require.NotNil(t, a)
	busprojects.SetLookup(a, resolveWorkload)
	return a, clients
}

// awaitConnected waits, within 30 s, until client's own connection is up.
func awaitConnected(t *testing.T, client *bus.Client) {
	t.Helper()
	require.Eventually(t, client.Connected, 30*time.Second, 20*time.Millisecond, "the hub-admission client never connected")
}

func replica(t *testing.T, report busprojects.Report, address string) busprojects.ReplicaReport {
	t.Helper()
	for _, r := range report.Listeners {
		if r.Address == address {
			return r
		}
	}
	t.Fatalf("the report has no replica %s: %+v", address, report.Listeners)
	return busprojects.ReplicaReport{}
}

// TestEveryListenerReplicaAnswersTheProof: the proof reads every replica of a workload, each at
// one of the addresses its instances name resolves to. It passes while all three answer 200 in
// required mode, from the workload's source, with a read that answered; it fails naming the
// replica for each way one of them can fail; and a workload whose name resolves to no address
// fails as UNKNOWN, naming the workload.
//
// Control: make readListeners read only the first address its instances resolve to, as a proof
// that called the workload's name once would reach one healthy replica; the broken replica goes
// unread, every broken row passes the proof, and the test fails naming 10.0.0.2.
func TestEveryListenerReplicaAnswersTheProof(t *testing.T) {
	hub := busprojectstest.StartHub(t, true)
	a, clients := admissionOver(t, hub)
	awaitConnected(t, clients[0])

	healthy := map[string]replicaAnswer{replicaHealthy1: answered("envoy-listener-a"), replicaBroken: answered("envoy-listener-b"), replicaHealthy3: answered("envoy-listener-c")}
	a.SetListeners(threeReplicas(healthy))
	require.NoError(t, a.Prove(t.Context()))
	require.True(t, a.Ready())
	report := a.Report()
	require.Len(t, report.Listeners, 3, "the proof did not read every replica %s resolves to (%s, %s, %s): %+v", workloadName, replicaHealthy1, replicaBroken, replicaHealthy3, report.Listeners)
	for i, address := range []string{replicaHealthy1, replicaBroken, replicaHealthy3} {
		require.Equal(t, busprojects.ReplicaReport{Workload: workload, Address: address, Instance: "envoy-listener-" + string(rune('a'+i)), Required: true, Source: workloadSource, Read: busprojects.ReadAnswered}, report.Listeners[i])
	}

	for _, row := range []struct {
		name   string
		answer replicaAnswer
		says   string
	}{
		{"one answering read failed", replicaAnswer{status: http.StatusServiceUnavailable, body: `{"instance":"envoy-listener-b","required":true,"source":"` + workloadSource + `","read":"failed"}`}, `answered 503, read "failed"`},
		{"one answering required false", replicaAnswer{status: http.StatusOK, body: `{"instance":"envoy-listener-b","required":false,"source":"","read":"none"}`}, "does not run in required privacy mode"},
		{"one naming another source", replicaAnswer{status: http.StatusOK, body: `{"instance":"envoy-listener-b","required":true,"source":"https://other.example.invalid","read":"answered"}`}, `reads "https://other.example.invalid", not its workload's source`},
		{"one not answering", replicaAnswer{noAnswer: true}, "did not answer"},
		{"one answering a body that does not parse", replicaAnswer{status: http.StatusOK, body: "the listener is starting"}, "answered a body that does not parse"},
	} {
		t.Run(row.name, func(t *testing.T) {
			answers := map[string]replicaAnswer{replicaHealthy1: healthy[replicaHealthy1], replicaBroken: row.answer, replicaHealthy3: healthy[replicaHealthy3]}
			a.SetListeners(threeReplicas(answers))
			err := a.Prove(t.Context())
			require.Error(t, err, "the proof passed with replica %s of %s broken: %s", replicaBroken, workload, row.name)
			require.ErrorContains(t, err, "listener "+workload+" replica "+replicaBroken+": "+row.says)
			require.NotContains(t, err.Error(), replicaHealthy1)
			require.NotContains(t, err.Error(), replicaHealthy3)
			require.False(t, a.Ready())
			report := a.Report()
			require.Equal(t, busprojects.ReadFailed, replica(t, report, replicaBroken).Read)
			require.Equal(t, busprojects.ReadAnswered, replica(t, report, replicaHealthy1).Read)
			require.Equal(t, busprojects.ReadAnswered, replica(t, report, replicaHealthy3).Read)
		})
	}

	t.Run("a workload whose name resolves to no address", func(t *testing.T) {
		targets, client := threeReplicas(healthy)
		targets = append(targets, busprojects.ListenerTarget{Name: "example-hub-listener", Source: workloadSource, Host: workloadHost, Instances: "example-hub-listener-instances.example.invalid:9020"})
		a.SetListeners(targets, client)
		err := a.Prove(t.Context())
		require.ErrorContains(t, err, "UNKNOWN: listener example-hub-listener: its instances \"example-hub-listener-instances.example.invalid:9020\" resolve to no address")
		require.False(t, a.Ready())
		last := a.Report().Listeners[len(a.Report().Listeners)-1]
		require.Equal(t, busprojects.ReplicaReport{Workload: "example-hub-listener", Read: busprojects.ReadUnknown, Error: `its instances "example-hub-listener-instances.example.invalid:9020" resolve to no address`}, last)
	})
}

// TestANoInventoryProofRefuses: an admission whose listeners were never set answers
// ErrNoListenerInventory, and is not ready, though its hub admits the proof; so does one given an
// empty inventory. Once given the ready listener, the same admission passes.
//
// Control: read a missing inventory as an empty one that passes (drop Prove's
// ErrNoListenerInventory refusal); the first row then passes the proof, and the test fails naming
// the missing inventory.
func TestANoInventoryProofRefuses(t *testing.T) {
	hub := busprojectstest.StartHub(t, true)
	a, clients := admissionOver(t, hub)
	awaitConnected(t, clients[0])

	err := a.Prove(t.Context())
	require.ErrorIs(t, err, busprojects.ErrNoListenerInventory, "a proof with no listener inventory passed")
	require.False(t, a.Ready())
	report := a.Report()
	require.Equal(t, busprojects.AdmissionAdmitted, report.Hubs[0].Admission, "the hubs are proven all the same: %+v", report.Hubs)
	require.Empty(t, report.Listeners)

	a.SetListeners(nil, nil)
	require.ErrorIs(t, a.Prove(t.Context()), busprojects.ErrNoListenerInventory, "a proof with an empty listener inventory passed")
	require.False(t, a.Ready())

	a.SetListeners(busprojectstest.ReadyListener(t))
	require.NoError(t, a.Prove(t.Context()))
	require.True(t, a.Ready())
}

// TestANilAdmissionMakesNothingPrivate: with no private hub, NewHubAdmission answers nil, whose
// Prove answers ErrNoBus, so a switch to private is refused; it is ready, since no project can be
// private there; Run returns at once; and its report, served with status 200, is empty.
func TestANilAdmissionMakesNothingPrivate(t *testing.T) {
	a := busprojects.NewHubAdmission(nil)
	require.Nil(t, a)
	require.ErrorIs(t, a.Prove(t.Context()), busprojects.ErrNoBus)
	require.True(t, a.Ready())
	done := make(chan struct{})
	go func() { a.Run(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run on a nil admission did not return")
	}
	recorder := httptest.NewRecorder()
	a.ReportHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/private-readiness", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"hubs":[],"listeners":[]}`, recorder.Body.String())
}

// TestAFailedProofIsReportedNotRefused: with one hub of two refusing and one listener replica's
// read failing, the proof fails, the report names that hub refused and that replica failed, and the
// report's handler still answers 200 with them: a failed proof is for the operator to read, and
// takes no Dispatch pod out of service.
//
// Control: make ReportHandler answer 503 while the report holds a failure, as a readiness probe's
// curl -f would read it; the test fails naming the 503.
func TestAFailedProofIsReportedNotRefused(t *testing.T) {
	tagged, untagged := busprojectstest.StartHub(t, true), busprojectstest.StartHub(t, false)
	a, clients := admissionOver(t, tagged, untagged)
	awaitConnected(t, clients[0])
	a.SetListeners(threeReplicas(map[string]replicaAnswer{
		replicaHealthy1: answered("envoy-listener-a"),
		replicaBroken:   {status: http.StatusServiceUnavailable, body: `{"instance":"envoy-listener-b","required":true,"source":"` + workloadSource + `","read":"failed"}`},
		replicaHealthy3: answered("envoy-listener-c"),
	}))

	err := a.Prove(t.Context())
	require.ErrorContains(t, err, "hub "+untagged.Server.URL())
	require.ErrorContains(t, err, "listener "+workload+" replica "+replicaBroken)
	require.False(t, a.Ready())

	recorder := httptest.NewRecorder()
	a.ReportHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/private-readiness", nil))
	require.Equal(t, http.StatusOK, recorder.Code, "the report of a failed proof answered %d: a probe reading it would take the pod out of service", recorder.Code)
	var served busprojects.Report
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &served))
	require.Equal(t, busprojects.AdmissionAdmitted, served.Hubs[0].Admission)
	require.Equal(t, busprojects.AdmissionRefused, served.Hubs[1].Admission)
	require.Contains(t, served.Hubs[1].Error, "not admitted")
	require.Equal(t, busprojects.ReadFailed, replica(t, served, replicaBroken).Read)
	require.Contains(t, untagged.Refusals(), busprojectstest.RefusedUntagged, "the untagged hub's auth service refused for another reason: %v", untagged.Refusals())
}

// TestTheGoLiveReportIsPublishedBeforeAnyProjectIsPrivate: whenever there is a private hub,
// whatever the database holds, Report lists every hub and every listener replica: before any proof
// each hub and workload unknown, and after one each hub's admission and each replica's read, a
// refusing hub as refused. It holds no project and no channel.
//
// Control: answer the empty report while no project is private, as a report published only
// once the database holds one would; the test fails naming the empty report.
func TestTheGoLiveReportIsPublishedBeforeAnyProjectIsPrivate(t *testing.T) {
	tagged, untagged := busprojectstest.StartHub(t, true), busprojectstest.StartHub(t, false)
	a, clients := admissionOver(t, tagged, untagged)
	awaitConnected(t, clients[0])
	a.SetListeners(threeReplicas(map[string]replicaAnswer{replicaHealthy1: answered("envoy-listener-a"), replicaBroken: answered("envoy-listener-b"), replicaHealthy3: answered("envoy-listener-c")}))

	before := a.Report()
	require.Equal(t, busprojects.Report{
		Hubs: []busprojects.HubReport{
			{URL: tagged.Server.URL(), Admission: busprojects.AdmissionUnknown},
			{URL: untagged.Server.URL(), Admission: busprojects.AdmissionUnknown},
		},
		Listeners: []busprojects.ReplicaReport{{Workload: workload, Read: busprojects.ReadUnknown}},
	}, before, "the report before any proof is not every hub and workload, unknown")

	require.Error(t, a.Prove(t.Context()))
	after := a.Report()
	require.False(t, after.ProvedAt.IsZero())
	require.Len(t, after.Hubs, 2, "the report after a proof is empty")
	require.Equal(t, busprojects.AdmissionAdmitted, after.Hubs[0].Admission)
	require.Equal(t, busprojects.AdmissionRefused, after.Hubs[1].Admission)
	require.Len(t, after.Listeners, 3)
	for _, address := range []string{replicaHealthy1, replicaBroken, replicaHealthy3} {
		require.Equal(t, busprojects.ReadAnswered, replica(t, after, address).Read)
	}
	served, err := json.Marshal(after)
	require.NoError(t, err)
	var fields any
	require.NoError(t, json.Unmarshal(served, &fields))
	for _, key := range jsonKeys(fields) {
		for _, word := range []string{"project", "channel"} {
			require.NotContains(t, strings.ToLower(key), word, "the report has a field %q: %s", key, served)
		}
	}
}

// jsonKeys is every object key of a decoded JSON value, at any depth.
func jsonKeys(value any) []string {
	var keys []string
	switch v := value.(type) {
	case map[string]any:
		for key, inner := range v {
			keys = append(append(keys, key), jsonKeys(inner)...)
		}
	case []any:
		for _, inner := range v {
			keys = append(keys, jsonKeys(inner)...)
		}
	}
	return keys
}

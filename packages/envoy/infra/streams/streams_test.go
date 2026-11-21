package streams_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/streams"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

func TestMain(m *testing.M) { testnats.Main(m) }

// hubConfig is the authorization of a test hub: an admin, every connection with no credential,
// and one user, subject, held to grant.
func hubConfig(grant natsgrant.Permissions) string {
	quote := func(subjects []string) string {
		quoted := make([]string, len(subjects))
		for index, subject := range subjects {
			quoted[index] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(quoted, ", ")
	}
	return fmt.Sprintf(`authorization {
  users = [
    { user: "admin", password: "admin" },
    { user: "subject", password: "subject", permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }
  ]
}
no_auth_user: "admin"
`, quote(grant.Publish), quote(grant.Subscribe))
}

// hub is a hub to run a command against: its URL, an admin connection that sets the scene and reads
// what the command left, and the record of every stream create and update asked of it.
type hub struct {
	url    string
	admin  *natsgo.Conn
	js     natsgo.JetStreamContext
	writes *testnats.StreamWrites
}

// startHub starts a lone server, or a cluster of n members when n > 1, whose user subject holds
// grant.
func startHub(t *testing.T, n int, grant natsgrant.Permissions) *hub {
	t.Helper()
	var url string
	if n > 1 {
		url = testnats.StartClusterConfig(t, n, hubConfig(grant)).URL()
	} else {
		_, url = testnats.StartConfig(t, "jetstream {}\n"+hubConfig(grant), []string{"-js"}, true)
	}
	admin := testnats.Connect(t, url)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream(natsgo.MaxWait(30 * time.Second))
	require.NoError(t, err)
	return &hub{url: url, admin: admin, js: js, writes: testnats.WatchStreamWrites(t, url)}
}

// connect connects as subject, asking on inbox, and hands every asynchronous error to errs.
func (h *hub) connect(t *testing.T, inbox string, errs chan<- error) *natsgo.Conn {
	t.Helper()
	options := []natsgo.Option{natsgo.UserInfo("subject", "subject"), natsgo.CustomInboxPrefix(inbox)}
	if errs != nil {
		options = append(options, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	}
	conn, err := natsgo.Connect(h.url, options...)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	return conn
}

// makeDeclared makes every protected stream of the declared set under settings as the admin.
func (h *hub) makeDeclared(t *testing.T, settings streamset.Settings) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	require.NoError(t, streamset.EnsureStreams(ctx, h.admin, settings))
}

// phase is a hold row that reads p.
func phase(p natsgrant.OperatorPhase) streams.Phase {
	return func(context.Context) (natsgrant.OperatorPhase, error) { return p, nil }
}

// measureInbox and operatorInbox are the inbox prefixes the measurement job's and the operator's
// grants subscribe.
const (
	measureInbox  = "_INBOX_envoy_measure"
	operatorInbox = natsgrant.OperatorInbox
)

func failures(report streams.Report) []string {
	var lines []string
	for _, failure := range report.Failures {
		lines = append(lines, failure.String())
	}
	return lines
}

func requireFailureNaming(t *testing.T, report streams.Report, stream, what string) {
	t.Helper()
	for _, failure := range report.Failures {
		if failure.Stream == stream && strings.Contains(failure.What, what) {
			return
		}
	}
	t.Fatalf("no failure names %s %q: %v", stream, what, failures(report))
}

// check, run unattended as the measurement job's user under the answer the auth service gives it
// (natsgrant.ForCallout of Measure), reports a missing KV_envoy_roles and exits 1, then a
// KV_envoy_roles at fewer copies than the deployment's three, and an ENVOY_MEASURE whose byte
// limit was changed, each named; and the server is asked for no stream create and no update.
func TestStreamsCheckChangesNothing(t *testing.T) {
	h := startHub(t, 3, natsgrant.ForCallout(natsgrant.Measure()))
	settings := streamset.Settings{Replicas: 3}
	h.makeDeclared(t, settings)
	measure, _ := streamset.Find(streamset.Declared(settings), streamset.MeasureStream)
	changed := measure.Config
	changed.MaxBytes = 2 << 20
	_, err := h.js.AddStream(&changed)
	require.NoError(t, err)
	require.NoError(t, h.js.DeleteKeyValue(bus.RoleClaimBucket))
	h.writes.Take(t)

	errs := make(chan error, 64)
	conn := h.connect(t, measureInbox, errs)
	report := streams.Check(t.Context(), conn, settings)
	require.Empty(t, report.Unknown)
	require.Equal(t, 1, report.ExitCode(), "%v", report.Lines())
	requireFailureNaming(t, report, "KV_"+bus.RoleClaimBucket, "missing")
	requireFailureNaming(t, report, streamset.MeasureStream, "max_bytes")
	require.Empty(t, h.writes.Take(t), "check asked for a stream write")

	_, err = h.js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage, Replicas: 1})
	require.NoError(t, err)
	h.writes.Take(t)
	report = streams.Check(t.Context(), conn, settings)
	require.Equal(t, 1, report.ExitCode(), "%v", report.Lines())
	requireFailureNaming(t, report, "KV_"+bus.RoleClaimBucket, "num_replicas is 1, want 3")
	require.Empty(t, h.writes.Take(t), "check asked for a stream write")
	info, err := h.js.StreamInfo("KV_" + bus.RoleClaimBucket)
	require.NoError(t, err)
	require.Equal(t, 1, info.Config.Replicas, "check raised the bucket")
	// A write the grant refuses never reaches the watch above, so the connection's own refusals are
	// read too: check sent no write at all, refused or not.
	require.NoError(t, conn.Flush())
	for {
		select {
		case err := <-errs:
			require.NotContains(t, err.Error(), "$JS.API.STREAM.", "check sent a stream write: %v", err)
			continue
		default:
		}
		break
	}
}

// check's STREAM.INFO requests carry no body, so no subjects_filter: it never asks for
// ENVOY_NOTIFICATIONS' stored subjects, where a private issue key appears.
func TestStreamsCheckListsNoSubjects(t *testing.T) {
	h := startHub(t, 1, natsgrant.ForCallout(natsgrant.Measure()))
	h.makeDeclared(t, streamset.Settings{})
	requests, err := h.admin.SubscribeSync("$JS.API.STREAM.INFO.>")
	require.NoError(t, err)
	require.NoError(t, h.admin.Flush())

	streams.Check(t.Context(), h.connect(t, measureInbox, nil), streamset.Settings{})
	require.NoError(t, h.admin.Flush())
	seen := 0
	for {
		msg, err := requests.NextMsg(200 * time.Millisecond)
		if err != nil {
			break
		}
		seen++
		var body map[string]any
		if len(msg.Data) > 0 {
			require.NoError(t, json.Unmarshal(msg.Data, &body), "%s", msg.Subject)
		}
		require.NotContains(t, body, "subjects_filter", "check's %s asked for the stream's stored subjects", msg.Subject)
	}
	require.Positive(t, seen, "no STREAM.INFO request was seen, so the check above read nothing")
}

// ensure, as the operator outside a hold (natsgrant.Operator of OperatorPhaseReleased), makes a
// missing ENVOY_MEASURE with the build's configuration, and the same grant refuses a create of
// KV_envoy_roles with a publish permissions violation and no answer. It refuses to run under any
// phase but released, changing nothing.
func TestStreamsEnsureCreatesOnlyTheMeasureStream(t *testing.T) {
	h := startHub(t, 1, natsgrant.Operator(natsgrant.OperatorPhaseReleased))
	errs := make(chan error, 16)
	conn := h.connect(t, operatorInbox, errs)

	_, err := streams.Ensure(t.Context(), conn, streamset.Settings{}, phase(natsgrant.OperatorPhaseHeld))
	require.ErrorIs(t, err, streams.ErrWrongPhase)
	require.ErrorContains(t, err, "held")
	require.Empty(t, h.writes.Take(t), "ensure under another phase asked for a write")

	report, err := streams.Ensure(t.Context(), conn, streamset.Settings{}, phase(natsgrant.OperatorPhaseReleased))
	require.NoError(t, err)
	require.Equal(t, 0, report.ExitCode(), "%v", report.Lines())
	require.Equal(t, []string{"$JS.API.STREAM.CREATE." + streamset.MeasureStream}, h.writes.Take(t))
	info, err := h.js.StreamInfo(streamset.MeasureStream)
	require.NoError(t, err)
	measure, _ := streamset.Find(streamset.Declared(streamset.Settings{}), streamset.MeasureStream)
	require.Empty(t, streamset.Drift(measure, streamset.Info{StreamInfo: *info}))

	roles, _ := streamset.Find(streamset.Declared(streamset.Settings{}), "KV_"+bus.RoleClaimBucket)
	_, err = conn.Request("$JS.API.STREAM.CREATE.KV_"+bus.RoleClaimBucket, mustJSON(t, roles.Config), 2*time.Second)
	require.Error(t, err, "the released operator's create of KV_envoy_roles was answered")
	awaitPublishViolation(t, errs, "$JS.API.STREAM.CREATE.KV_"+bus.RoleClaimBucket)
	_, err = h.js.StreamInfo("KV_" + bus.RoleClaimBucket)
	require.ErrorIs(t, err, natsgo.ErrStreamNotFound, "the released operator made KV_envoy_roles")
}

// ensure, as the operator outside a hold, brings a deployed ENVOY_MEASURE to its declared
// settings: found at one copy on a hub whose deployment keeps three, it is updated, with the one
// update call of ENVOY_MEASURE, to three copies, and check then reports nothing of it. A second
// ensure finds nothing to bring and sends nothing.
func TestStreamsEnsureRaisesTheMeasureStreamsCopies(t *testing.T) {
	h := startHub(t, 3, natsgrant.Operator(natsgrant.OperatorPhaseReleased))
	settings := streamset.Settings{Replicas: 3}
	measure, _ := streamset.Find(streamset.Declared(settings), streamset.MeasureStream)
	oneCopy := measure.Config
	oneCopy.Replicas = 1
	_, err := h.js.AddStream(&oneCopy)
	require.NoError(t, err)
	h.writes.Take(t)
	conn := h.connect(t, operatorInbox, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	report, err := streams.Ensure(ctx, conn, settings, phase(natsgrant.OperatorPhaseReleased))
	require.NoError(t, err)
	require.Equal(t, 0, report.ExitCode(), "%v", report.Lines())
	require.Equal(t, []string{"$JS.API.STREAM.UPDATE." + streamset.MeasureStream}, h.writes.Take(t))
	info := testnats.AwaitStreamCopies(t, h.js, streamset.MeasureStream, 3)
	require.Equal(t, 3, info.Config.Replicas, "ensure left ENVOY_MEASURE below the deployment's copies")
	require.Empty(t, streamset.Drift(measure, streamset.Info{StreamInfo: *info}), "ENVOY_MEASURE still drifts after ensure")

	report, err = streams.Ensure(ctx, conn, settings, phase(natsgrant.OperatorPhaseReleased))
	require.NoError(t, err)
	require.Equal(t, []string{"CLEAN"}, report.Lines())
	require.Empty(t, h.writes.Take(t), "a second ensure sent a write")
}

// update, as the operator inside a hold (natsgrant.Operator of OperatorPhaseHeld), makes a missing
// KV_envoy_roles at the deployment's three copies, raises a bucket found at one copy to three, and
// stops, naming the stream and the setting, at KV_envoy_ci_state, whose subjects differ from the
// build's: their subjects are left as they are, and no stream after it is touched. It refuses to
// run outside a hold.
func TestStreamsUpdateChangesOnlyReplicas(t *testing.T) {
	h := startHub(t, 3, natsgrant.Operator(natsgrant.OperatorPhaseHeld))
	h.makeDeclared(t, streamset.Settings{Replicas: 1})
	require.NoError(t, h.js.DeleteKeyValue(bus.RoleClaimBucket))
	ciState := "KV_" + bus.CIStateBucket
	declaredCI, _ := streamset.Find(streamset.Declared(streamset.Settings{}), ciState)
	require.NoError(t, h.js.DeleteStream(ciState))
	odd := declaredCI.Config
	odd.Subjects = append(slices.Clone(odd.Subjects), "odd.>")
	_, err := h.js.AddStream(&odd)
	require.NoError(t, err)
	h.writes.Take(t)
	conn := h.connect(t, operatorInbox, nil)
	settings := streamset.Settings{Replicas: 3}

	_, err = streams.Update(t.Context(), conn, settings, phase(natsgrant.OperatorPhaseReleased))
	require.ErrorIs(t, err, streams.ErrWrongPhase)
	require.Empty(t, h.writes.Take(t), "update outside a hold asked for a write")

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	report, err := streams.Update(ctx, conn, settings, phase(natsgrant.OperatorPhaseHeld))
	require.NoError(t, err)
	require.Empty(t, report.Unknown)
	require.Equal(t, 1, report.ExitCode(), "%v", report.Lines())
	requireFailureNaming(t, report, ciState, "subjects is")
	for _, bucket := range []string{bus.RoleClaimBucket, bus.InterestBucket, bus.SessionBucket} {
		info := testnats.AwaitStreamCopies(t, h.js, "KV_"+bucket, 3)
		require.Equal(t, 3, info.Config.Replicas, "KV_%s", bucket)
	}
	info, err := h.js.StreamInfo(ciState)
	require.NoError(t, err)
	require.Equal(t, odd.Subjects, info.Config.Subjects, "update changed the subjects of a stream it stops at")
	require.Equal(t, 1, info.Config.Replicas, "update raised the stream it stops at")
	dpop, err := h.js.StreamInfo("KV_" + bus.DPoPProofBucket)
	require.NoError(t, err)
	require.Equal(t, 1, dpop.Config.Replicas, "update went on past the stream it stopped at")
	for _, write := range h.writes.Take(t) {
		require.False(t, strings.HasSuffix(write, "."+ciState), "update wrote %s", write)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

// awaitPublishViolation waits for the server's publish permissions violation naming subject among
// errs.
func awaitPublishViolation(t *testing.T, errs <-chan error, subject string) {
	t.Helper()
	want := `Permissions Violation for Publish to "` + subject + `"`
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-errs:
			if strings.Contains(err.Error(), want) {
				return
			}
		case <-deadline:
			t.Fatalf("no %s", want)
		}
	}
}

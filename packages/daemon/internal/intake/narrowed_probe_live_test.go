package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/natsgrant"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// probeCreateSubject is the one call the grant names on CAPTURE's probe: its create, on CAPTURE's
// issue events.
const probeCreateSubject = "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.legion-go-CAPTURE-dispatch-probe.notifications.dispatch.issue.CAPTURE.>"

// openNarrowed is OpenConsumers under the rig's narrowed daemon, through a lagStandIn whose
// uncounted can make the Dispatch durable's counts read zero, with the consumers' logs.
func (r *narrowedRig) openNarrowed(t *testing.T) (*Consumers, *lagStandIn, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	standIn := &lagStandIn{JetStream: r.daemon}
	read, cancel := rowRead(t)()
	defer cancel()
	consumers, err := OpenConsumers(read, standIn, r.pool, consumerSpec(logs))
	if err != nil {
		t.Fatalf("OpenConsumers under the narrowed grant: %v", err)
	}
	return consumers, standIn, logs
}

// readPosition is DispatchPosition within the row's bound.
func readPosition(t *testing.T, consumers *Consumers) DispatchConsumerPosition {
	t.Helper()
	read, cancel := rowRead(t)()
	defer cancel()
	position, err := consumers.DispatchPosition(read)
	if err != nil {
		t.Fatalf("DispatchPosition: %v", err)
	}
	return position
}

// A daemon held to its projects confirms its Dispatch consumer idle with its probe's create alone,
// the one call WithinProjects grants on the probe: with nothing waiting, the server routes the
// create and the reading is a confirmed idle; with a CAPTURE message stored behind counts reading
// zero, the create counts it and the reading is not idle. Control: the same grant without the
// probe's create is refused it, which the server reports for that subject, and the reading is not
// idle and says the idle could not be confirmed, so nothing held is released (Reached).
func TestANarrowedDaemonConfirmsItsIdleWithItsProbeAlone(t *testing.T) {
	t.Run("the held grant", func(t *testing.T) {
		rig := newNarrowedRig(t)
		consumers, standIn, logs := rig.openNarrowed(t)
		rig.daemonCalls()
		if position := readPosition(t, consumers); !position.Idle || strings.Contains(logs.String(), "idle could not be confirmed") {
			t.Fatalf("idle = %v under the held grant with nothing waiting, want a confirmed idle:\n%s", position.Idle, logs)
		}
		if calls := rig.daemonCalls(); !slices.Contains(calls, probeCreateSubject) || rig.refused.of(probeCreateSubject) {
			t.Fatalf("the daemon's routed calls %q and refusals %q, want the probe's create routed and not refused", calls, rig.refused.all())
		}
		waitProbeGone(t, rig.stream, consumers.spec.Project)
		publish(t, rig.admin, "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
		testwait.Eventually(t, "the stored message counted on the durable", func() bool {
			return consumerInfo(t, rig.stream, dispatchConsumerName(consumers.spec.Project)).NumPending == 1
		})
		standIn.uncounted = true
		if position := readPosition(t, consumers); position.Idle || position.Reached() || strings.Contains(logs.String(), "idle could not be confirmed") {
			t.Fatalf("idle = %v with a CAPTURE message stored behind counts reading zero, want a confirmed not idle:\n%s", position.Idle, logs)
		}
	})

	t.Run("control: the grant without the probe's create", func(t *testing.T) {
		rig := newNarrowedRigWith(t, func(grant *natsgrant.Permissions) {
			held := len(grant.Publish)
			grant.Publish = slices.DeleteFunc(grant.Publish, func(subject string) bool { return subject == probeCreateSubject })
			if len(grant.Publish) != held-1 {
				t.Fatalf("WithinProjects grants no %s, so the control removes nothing: %q", probeCreateSubject, grant.Publish)
			}
		})
		consumers, _, logs := rig.openNarrowed(t)
		position := readPosition(t, consumers)
		if position.Idle || position.Reached() || !strings.Contains(logs.String(), "idle could not be confirmed") {
			t.Fatalf("idle = %v without the probe's create, want not idle and the line that says so:\n%s", position.Idle, logs)
		}
		testwait.Eventually(t, "the server's refusal of the probe's create", func() bool { return rig.refused.of(probeCreateSubject) })
	})
}

// Over a boot, two idle readings, a lag read, two failure sweeps, a remake of the Dispatch durable
// at its stored position and a run that applies a fact, a daemon held to its projects sends no
// request on the stream's own API (STREAM.INFO, STREAM.MSG.GET, STREAM.LIST, STREAM.NAMES and
// every other $JS.API.STREAM call) and no direct get ($JS.API.DIRECT.GET), whether the server
// routed it or refused it: what it sends is consumer calls by name and the probe's create.
//
// Two instruments read what it sends: the server's refusals of its connection, and an admin's
// subscription on $JS.API.>, which the server copies every request it routes to. Controls, so a
// count of none is not an instrument that saw nothing: the subscription saw the probe's create, the
// sweep's and the lag read's consumer info and the remake's create; and, after the phases, the
// daemon's own stream info, stored-message get and direct get are each refused by name, the last
// of them read as the barrier behind which every earlier refusal has arrived, while the admin's
// same three calls are answered: the stream info with PRIV's subject among those it counts, and
// both reads with the PRIV message seeded first.
func TestANarrowedDaemonSendsNoStreamCall(t *testing.T) {
	ctx := context.Background()
	rig := newNarrowedRig(t)
	seeded := publish(t, rig.admin, "notifications.dispatch.issue.PRIV.PRIV-1.issue.updated", envelopeJSON(t, "dispatch-priv-1", "dispatch",
		`{"id":1,"issue_key":"PRIV-1","seq":1,"notify":true,"type":"issue.updated","payload":{"title":"a private title"}}`))
	var calls []string

	// The boot, two idle readings and a lag read.
	consumers, _, logs := rig.openNarrowed(t)
	project := consumers.spec.Project
	dispatch := dispatchConsumerName(project)
	for range 2 {
		if position := readPosition(t, consumers); !position.Idle {
			t.Fatalf("not idle under the held grant with nothing of CAPTURE stored:\n%s", logs)
		}
		waitProbeGone(t, rig.stream, project)
	}
	for _, lag := range consumers.Lag(ctx) {
		if lag.Unavailable != "" {
			t.Fatalf("Lag of %s under the narrowed grant: %s", lag.Name, lag.Unavailable)
		}
	}
	// Two sweeps over the four durables.
	sweep := &failureSweep{consumers: consumers}
	sweep.pass(ctx)
	sweep.pass(ctx)
	calls = append(calls, rig.daemonCalls()...)
	for _, want := range []string{probeCreateSubject, "$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS." + dispatch, "$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS." + readyConsumerName(project)} {
		if !slices.Contains(calls, want) {
			t.Fatalf("the subscription saw the daemon send %q, want %s among them: it would see nothing", calls, want)
		}
	}

	// A fact applied, so the durable keeps a position, then the durable deleted and made again at it.
	stop := runConsumers(t, consumers, marks())
	applied := publish(t, rig.admin, "notifications.dispatch.issue.CAPTURE.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
	assertAcknowledgedThrough(t, rig.stream, dispatch, applied)
	stop()
	if err := rig.stream.DeleteConsumer(ctx, dispatch); err != nil {
		t.Fatalf("delete the Dispatch durable: %v", err)
	}
	remade, _, remadeLogs := rig.openNarrowed(t)
	if !strings.Contains(remadeLogs.String(), "created at the last message it applied") {
		t.Fatalf("the remake did not start the durable at its stored position:\n%s", remadeLogs)
	}
	stop = runConsumers(t, remade, marks())
	assertAcknowledgedThrough(t, rig.stream, dispatch, publish(t, rig.admin, "notifications.dispatch.issue.CAPTURE.CAPTURE-5.issue.created",
		envelopeWithID(t, capturedIssueCreatedEnvelope(t), "dispatch-capture-5")))
	stop()
	calls = append(calls, rig.daemonCalls()...)
	if !slices.Contains(calls, "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS."+dispatch+".notifications.dispatch.issue.CAPTURE.>") {
		t.Fatalf("the subscription saw the daemon send %q, want the remake's create of %s among them", calls, dispatch)
	}

	// The daemon's own stream calls, the controls: each refused by name.
	streamInfo, msgGet, directGet := "$JS.API.STREAM.INFO."+notificationStream, "$JS.API.STREAM.MSG.GET."+notificationStream, "$JS.API.DIRECT.GET."+notificationStream
	if _, err := rig.admin.UpdateStream(ctx, jetstream.StreamConfig{Name: notificationStream, Subjects: []string{"notifications.>"}, AllowDirect: true}); err != nil {
		t.Fatalf("let the stream answer direct gets, so the admin's control is answered: %v", err)
	}
	for _, subject := range []string{streamInfo, msgGet, directGet} {
		ask, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := rig.daemonConn.RequestWithContext(ask, subject, []byte(fmt.Sprintf(`{"seq":%d}`, seeded.Sequence)))
		cancel()
		if err == nil {
			t.Fatalf("the narrowed daemon was answered %s", subject)
		}
	}
	testwait.Eventually(t, "the server's refusal of the daemon's direct get, the last call it sent", func() bool { return rig.refused.of(directGet) })
	for _, subject := range []string{streamInfo, msgGet} {
		if !rig.refused.of(subject) {
			t.Fatalf("the server's refusals %q name no %s: the refusals would not show the daemon's own stream calls", rig.refused.all(), subject)
		}
	}

	// What the phases sent, before the controls: no stream call and no direct get, routed or refused.
	stateCall := func(subject string) bool {
		return strings.HasPrefix(subject, "$JS.API.STREAM.") || strings.HasPrefix(subject, "$JS.API.DIRECT.")
	}
	if sent := slices.DeleteFunc(slices.Clone(calls), func(subject string) bool { return !stateCall(subject) }); len(sent) != 0 {
		t.Fatalf("the narrowed daemon sent stream calls the server routed: %q", sent)
	}
	controls := map[string]bool{streamInfo: true, msgGet: true, directGet: true}
	for _, line := range rig.refused.all() {
		for _, field := range strings.Fields(line) {
			subject := strings.Trim(field, `"`)
			if stateCall(subject) && !controls[subject] {
				t.Fatalf("the server refused the narrowed daemon a stream call over the phases: %s", line)
			}
		}
	}

	// The admin's same calls are answered, with PRIV's data.
	info, err := rig.stream.Info(ctx, jetstream.WithSubjectFilter(">"))
	if err != nil {
		t.Fatalf("the admin's stream info: %v", err)
	}
	if _, ok := info.State.Subjects["notifications.dispatch.issue.PRIV.PRIV-1.issue.updated"]; !ok {
		t.Fatalf("the admin's stream info counts %v, want PRIV-1's subject among them: the stream info shows what a daemon held to its projects may not read", info.State.Subjects)
	}
	for _, subject := range []string{msgGet, directGet} {
		reply, err := rig.adminConn.Request(subject, []byte(fmt.Sprintf(`{"seq":%d}`, seeded.Sequence)), 5*time.Second)
		if err != nil {
			t.Fatalf("the admin's %s: %v", subject, err)
		}
		body := reply.Data
		if subject == msgGet {
			var answer struct {
				Message struct {
					Data []byte `json:"data"`
				} `json:"message"`
			}
			if err := json.Unmarshal(reply.Data, &answer); err != nil {
				t.Fatalf("read the admin's %s: %v: %s", subject, err, reply.Data)
			}
			body = answer.Message.Data
		}
		if !strings.Contains(string(body), "a private title") {
			t.Fatalf("the admin's %s answered %q, want the PRIV message seeded first", subject, reply.Data)
		}
	}
}

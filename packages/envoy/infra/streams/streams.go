// Package streams is envoy-infra's streams commands: every JetStream stream Envoy declares
// (internal/streamset) is made at deploy time by the operator, since beside the NATS auth service no
// other client may create or change one (natsgrant.ForCallout) and every start there only reads its
// streams (bus.StreamsOperatorMadeEnvVar). Three functions, one per word of `envoy-infra streams`.
// Check is this package's own word (Command). Update and Census run inside a hold: the writer hold
// registers `streams update` and `streams census` as its held commands, runs these functions
// inside a hold of their workflow, and writes the hold journal's intent, done and stop entries.
//
//   - Check, run by each deploy, unattended, as the measurement job's user: it reads each declared
//     stream and reports what drifted, naming the stream, and sends no create and no update.
//   - Update, as the operator inside a hold: it makes a missing protected stream, or a missing
//     ENVOY_MEASURE, with the build's configuration and raises one by today's reconcile rules, and
//     stops on any other difference.
//   - Census, as the operator inside a hold: it lists every stream and consumer and removes what the
//     declared set does not name, and puts back the configuration of a declared stream a read
//     through another stream could use (Census).
//
// Each operator command reads the hold's phase first and refuses, naming it, any phase but its own
// (ErrWrongPhase), so a deploy during a hold changes nothing.
package streams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/natsgrant"
)

// Phase reads the hold row's phase, verified (the operator's hold client, holdrecord.Client.Row):
// the one input that admits an operator command.
type Phase func(ctx context.Context) (natsgrant.OperatorPhase, error)

// ErrWrongPhase is an operator command refused because the hold row is not in the command's phase:
// Update and Census run only while it is held.
var ErrWrongPhase = errors.New("the hold row is not in this command's phase")

// requirePhase reads the row through phase and refuses, naming both phases, unless it is want.
func requirePhase(ctx context.Context, command string, phase Phase, want natsgrant.OperatorPhase) error {
	if phase == nil {
		return fmt.Errorf("%s: %w: no reader of the hold row", command, ErrWrongPhase)
	}
	got, err := phase(ctx)
	if err != nil {
		return fmt.Errorf("%s: read the hold row: %w", command, err)
	}
	if got != want {
		return fmt.Errorf("%s: %w: the row is %s, and %s runs only while it is %s", command, ErrWrongPhase, got, command, want)
	}
	return nil
}

// Finding is one thing a command found: a stream or consumer, named, and what is wrong with it or
// what the command did to it.
type Finding struct {
	// Stream is the stream's name, and Consumer the consumer's, empty for a stream's finding.
	Stream, Consumer string
	// What is the finding: "missing", a setting's difference, "made", "raised", "removed" and so on.
	What string
}

func (f Finding) String() string {
	if f.Consumer != "" {
		return fmt.Sprintf("%s consumer %s: %s", f.Stream, f.Consumer, f.What)
	}
	return f.Stream + ": " + f.What
}

// Report is what one command found and did.
type Report struct {
	// Failures are what makes the run fail, each named: a missing stream, a drift, an undeclared
	// stream or consumer.
	Failures []Finding
	// Changes are what the run made, raised, updated or removed.
	Changes []Finding
	// Unknown is what the run could not read, each naming why: a run with any is UNKNOWN, never
	// clean.
	Unknown []string
}

// ExitCode is the command's exit code for r: 2 when it read something it could not read (UNKNOWN),
// 1 when it found a failure, and 0 for a clean run.
func (r Report) ExitCode() int {
	switch {
	case len(r.Unknown) > 0:
		return 2
	case len(r.Failures) > 0:
		return 1
	}
	return 0
}

// Lines are r as the command prints it: one line for each change, failure and unknown read, and a
// last line, CLEAN, FAIL or UNKNOWN.
func (r Report) Lines() []string {
	var lines []string
	for _, change := range r.Changes {
		lines = append(lines, "CHANGED "+change.String())
	}
	for _, failure := range r.Failures {
		lines = append(lines, "FAIL "+failure.String())
	}
	for _, unknown := range r.Unknown {
		lines = append(lines, "UNKNOWN "+unknown)
	}
	switch r.ExitCode() {
	case 2:
		return append(lines, "UNKNOWN")
	case 1:
		return append(lines, "FAIL")
	}
	return append(lines, "CLEAN")
}

// requestWait bounds each JetStream API request a command sends.
const requestWait = 10 * time.Second

// streamInfo reads name's info on conn: STREAM.INFO with no body, so it never asks for the
// stream's stored subjects (subjects_filter), where a private issue key appears on
// ENVOY_NOTIFICATIONS. A stream that does not exist is streamset.ErrStreamNotFound.
func streamInfo(ctx context.Context, conn *nats.Conn, name string) (streamset.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, requestWait)
	defer cancel()
	answer, err := conn.RequestWithContext(ctx, "$JS.API.STREAM.INFO."+name, nil)
	if err != nil {
		return streamset.Info{}, fmt.Errorf("STREAM.INFO %s: %w", name, err)
	}
	return streamset.DecodeInfo(answer.Data)
}

// apiError is the error a JetStream API answer carries, nil for none.
type apiError struct {
	Code        int    `json:"code"`
	ErrCode     int    `json:"err_code"`
	Description string `json:"description"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s (%d, err_code %d)", e.Description, e.Code, e.ErrCode)
}

// request sends body, JSON, on the JetStream API subject subject and returns the answer's body, or
// the error the answer carries.
func request(ctx context.Context, conn *nats.Conn, subject string, body any) ([]byte, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, requestWait)
	defer cancel()
	answer, err := conn.RequestWithContext(ctx, subject, raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimPrefix(subject, "$JS.API."), err)
	}
	var carried struct {
		Error *apiError `json:"error"`
	}
	if err := json.Unmarshal(answer.Data, &carried); err != nil {
		return nil, fmt.Errorf("%s: read the answer: %w", strings.TrimPrefix(subject, "$JS.API."), err)
	}
	if carried.Error != nil {
		return nil, fmt.Errorf("%s: %w", strings.TrimPrefix(subject, "$JS.API."), carried.Error)
	}
	return answer.Data, nil
}

// Check reads each declared stream under settings on conn and reports, naming the stream, one that
// is missing, one with fewer copies than the deployment's count, and every other difference from
// the build's configuration (streamset.Drift). The hold's barrier, which the hold makes and removes,
// is not read. It sends no create and no update: it runs, unattended, as the measurement job's user
// (natsgrant.Measure, answered through ForCallout), whose grant holds every stream's info and no
// write. A read that draws no answer, or that the grant refuses, is UNKNOWN, never a pass.
func Check(ctx context.Context, conn *nats.Conn, settings streamset.Settings) Report {
	var report Report
	for _, stream := range streamset.Declared(settings) {
		if stream.Transient {
			continue
		}
		info, err := streamInfo(ctx, conn, stream.Name)
		switch {
		case errors.Is(err, streamset.ErrStreamNotFound):
			report.Failures = append(report.Failures, Finding{Stream: stream.Name, What: "missing"})
			continue
		case err != nil:
			report.Unknown = append(report.Unknown, err.Error())
			continue
		}
		for _, difference := range streamset.Drift(stream, info) {
			report.Failures = append(report.Failures, Finding{Stream: stream.Name, What: difference.String()})
		}
	}
	return report
}

// Update makes every protected stream of the declared set, and ENVOY_MEASURE, that is missing on
// conn, with the build's configuration, and raises each that is there by today's reconcile rules
// (streamset.Ensure): the notification stream's subjects only added, its limits only raised and
// its envoy_stream_id kept; a bucket's copies raised and the session bucket's age set;
// ENVOY_MEASURE's copies raised. It stops, naming the stream and the setting, at the first stream
// with any other difference (streamset.Difference.Raise), which only a census or a restore puts
// right. It runs as the operator inside a hold, whose grant holds those creates and updates, and
// refuses to run unless phase reads the hold row held (ErrWrongPhase). No grant outside a hold
// holds a stream create or update, ENVOY_MEASURE's included: a create or an update carries a
// configuration of the caller's choosing, whose republish could write any subject, so this is
// ENVOY_MEASURE's one maker. The barrier is not its.
func Update(ctx context.Context, conn *nats.Conn, settings streamset.Settings, phase Phase) (Report, error) {
	if err := requirePhase(ctx, "streams update", phase, natsgrant.OperatorPhaseHeld); err != nil {
		return Report{}, err
	}
	js, err := conn.JetStream(nats.MaxWait(requestWait))
	if err != nil {
		return Report{}, err
	}
	var report Report
	for _, stream := range streamset.Declared(settings) {
		if stream.Transient || !(stream.Protected || stream.Name == streamset.MeasureStream) {
			continue
		}
		info, err := streamInfo(ctx, conn, stream.Name)
		missing := errors.Is(err, streamset.ErrStreamNotFound)
		if err != nil && !missing {
			report.Unknown = append(report.Unknown, err.Error())
			return report, nil
		}
		var raise []streamset.Difference
		if !missing {
			for _, difference := range streamset.Drift(stream, info) {
				if !difference.Raise {
					report.Failures = append(report.Failures, Finding{Stream: stream.Name, What: difference.String() + ": update stops here; a census or a restore puts it right"})
					return report, nil
				}
				raise = append(raise, difference)
			}
			if len(raise) == 0 {
				continue
			}
		}
		if err := streamset.Ensure(ctx, conn, js, stream); err != nil {
			report.Failures = append(report.Failures, Finding{Stream: stream.Name, What: err.Error()})
			return report, nil
		}
		what := "made"
		if !missing {
			what = "raised: " + differences(raise)
		}
		report.Changes = append(report.Changes, Finding{Stream: stream.Name, What: what})
	}
	return report, nil
}

func differences(drift []streamset.Difference) string {
	parts := make([]string, len(drift))
	for index, difference := range drift {
		parts[index] = difference.String()
	}
	return strings.Join(parts, "; ")
}

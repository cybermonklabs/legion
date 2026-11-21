package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The cutover reads (`legion cutover-read drain|filters`, plan C's t-c-10): readings an operator
// takes around the deploy that drops the shared wake filter, from a connection with the in-cluster
// listener's grant. They read and never write: the stream's info, its consumer list and stored
// messages by sequence. They make no call on a daemon's durable, neither a create, an update, a
// delete nor an acknowledgement, so no reading moves a daemon's position.

// The cutover reads' verdicts, each the last line a reading prints.
const (
	GateOpen    = "GATE OPEN"
	GateShut    = "GATE SHUT"
	GateUnknown = "UNKNOWN"
)

// cutoverReadBound bounds each read a cutover read makes.
const cutoverReadBound = 10 * time.Second

// CutoverReading is one reading: a line per project or durable, and the verdict, which is also the
// last line the command prints. Open is true only on GATE OPEN.
type CutoverReading struct {
	Lines   []string
	Verdict string
}

// Open says whether the reading is GATE OPEN.
func (r CutoverReading) Open() bool { return r.Verdict == GateOpen }

// CutoverReader reads the notification stream for the cutover reads, through js, the reader's own
// connection.
type CutoverReader struct {
	js jetstream.JetStream
}

// NewCutoverReader is a cutover reader on js.
func NewCutoverReader(js jetstream.JetStream) *CutoverReader {
	return &CutoverReader{js: js}
}

// consumers is every consumer of the notification stream, read whole (CONSUMER.LIST, page by
// page), by name.
func (r *CutoverReader) consumers(ctx context.Context) (map[string]jetstream.ConsumerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, cutoverReadBound)
	defer cancel()
	stream, err := r.js.Stream(ctx, notificationStream)
	if err != nil {
		return nil, fmt.Errorf("read the stream %s: %w", notificationStream, err)
	}
	list := stream.ListConsumers(ctx)
	byName := map[string]jetstream.ConsumerInfo{}
	for info := range list.Info() {
		byName[info.Name] = *info
	}
	if err := list.Err(); err != nil {
		return nil, fmt.Errorf("list the consumers of %s: %w", notificationStream, err)
	}
	return byName, nil
}

// daemonDurableProject is the project a daemon durable's name gives, legion-go-<P>-<kind>, and
// its kind, or false for any other name.
func daemonDurableProject(name string) (project, kind string, ok bool) {
	rest, found := strings.CutPrefix(name, "legion-go-")
	if !found {
		return "", "", false
	}
	cut := strings.LastIndex(rest, "-")
	if cut <= 0 || cut == len(rest)-1 {
		return "", "", false
	}
	return rest[:cut], rest[cut+1:], true
}

// filtersOf is a consumer's filter set, whichever field the server answered it in.
func filtersOf(info jetstream.ConsumerInfo) []string {
	filters := slices.Clone(info.Config.FilterSubjects)
	if info.Config.FilterSubject != "" {
		filters = append([]string{info.Config.FilterSubject}, filters...)
	}
	return filters
}

// sameSet says whether two filter lists hold the same subjects.
func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// projectsToJudge is the projects named, each once, and every project of a durable of kind on the
// stream that was not named: a durable of a project the operator left out is judged too.
func projectsToJudge(projects []string, consumers map[string]jetstream.ConsumerInfo, kinds ...string) []string {
	judged := slices.Clone(projects)
	for name := range consumers {
		if project, kind, ok := daemonDurableProject(name); ok && slices.Contains(kinds, kind) && !slices.Contains(judged, project) {
			judged = append(judged, project)
		}
	}
	slices.Sort(judged)
	return slices.Compact(judged)
}

// verdictOf is a reading's verdict from its per-project states: GATE SHUT when any is shut, else
// UNKNOWN when any is unknown or none was judged, else GATE OPEN.
func verdictOf(shut, unknown, judged int) string {
	switch {
	case shut > 0:
		return GateShut
	case unknown > 0 || judged == 0:
		return GateUnknown
	default:
		return GateOpen
	}
}

// drainStart is where the drain read starts on the shared subject for a durable whose ack floor is
// floor: just past it, since a wake delivered and not yet acknowledged has not been applied.
func drainStart(floor, _ uint64) uint64 { return floor + 1 }

// Drain is plan C's drain read, step 3 of the wake migration: for each project, the wake durable
// (legion-go-<P>-wake) must be in deploy 1's shape, on the shared wake subject and the project's
// own, and every message stored on the shared subject from just past its ack floor to the stream's
// last sequence must be a listener_ready report. A project prints DRAINED, NOT DRAINED naming the
// sequence and kind of the first wake still unread, or UNKNOWN naming the read it could not make
// (no wake durable, a durable in another shape, a refused or failed read, a message that does not
// decode). No project named is UNKNOWN; every wake durable on the stream is judged, named or not.
func (r *CutoverReader) Drain(ctx context.Context, projects []string) CutoverReading {
	return r.drain(ctx, projects, drainStart)
}

func (r *CutoverReader) drain(ctx context.Context, projects []string, start func(floor, last uint64) uint64) CutoverReading {
	if len(projects) == 0 {
		return CutoverReading{Lines: []string{"UNKNOWN: no project named: --projects is empty"}, Verdict: GateUnknown}
	}
	consumers, err := r.consumers(ctx)
	if err != nil {
		return CutoverReading{Lines: []string{"UNKNOWN: " + err.Error()}, Verdict: GateUnknown}
	}
	last, err := r.lastSequence(ctx)
	if err != nil {
		return CutoverReading{Lines: []string{"UNKNOWN: " + err.Error()}, Verdict: GateUnknown}
	}
	var reading CutoverReading
	var shut, unknown int
	judged := projectsToJudge(projects, consumers, "wake")
	for _, project := range judged {
		line, state := r.drainProject(ctx, project, consumers, last, start)
		reading.Lines = append(reading.Lines, line)
		switch state {
		case GateShut:
			shut++
		case GateUnknown:
			unknown++
		}
	}
	reading.Verdict = verdictOf(shut, unknown, len(judged))
	return reading
}

// drainProject reads one project's wake durable and the shared subject past its floor.
func (r *CutoverReader) drainProject(ctx context.Context, project string, consumers map[string]jetstream.ConsumerInfo, last uint64, start func(floor, last uint64) uint64) (string, string) {
	name := wakeConsumerName(project)
	info, found := consumers[name]
	if !found {
		return fmt.Sprintf("UNKNOWN %s: the stream holds no wake durable %s", project, name), GateUnknown
	}
	own, err := wakeSubject(project)
	if err != nil {
		return fmt.Sprintf("UNKNOWN %s: %v", project, err), GateUnknown
	}
	if filters := filtersOf(info); !sameSet(filters, []string{legionWakeSubject, own}) {
		return fmt.Sprintf("UNKNOWN %s: %s filters %s, not deploy 1's shape, %s and %s", project, name, strings.Join(filters, ", "), legionWakeSubject, own), GateUnknown
	}
	floor := info.AckFloor.Stream
	read := 0
	for seq := start(floor, last); seq <= last; {
		message, found, err := r.storedWake(ctx, seq)
		if err != nil {
			return fmt.Sprintf("UNKNOWN %s: read %s at or after sequence %d: %v", project, legionWakeSubject, seq, err), GateUnknown
		}
		if !found || message.Sequence > last {
			break
		}
		kind, err := wakeKindOf(message.Data)
		if err != nil {
			return fmt.Sprintf("UNKNOWN %s: the message at sequence %d does not decode: %v", project, message.Sequence, err), GateUnknown
		}
		if kind != ListenerWakeReady {
			return fmt.Sprintf("NOT DRAINED %s: %s holds %s at sequence %d, past %s's ack floor %d", project, legionWakeSubject, kind, message.Sequence, name, floor), GateShut
		}
		read++
		seq = message.Sequence + 1
	}
	return fmt.Sprintf("DRAINED %s: %d listener_ready past %s's ack floor %d, to the stream's last sequence %d", project, read, name, floor, last), GateOpen
}

// lastSequence is the stream's last sequence (STREAM.INFO).
func (r *CutoverReader) lastSequence(ctx context.Context) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, cutoverReadBound)
	defer cancel()
	stream, err := r.js.Stream(ctx, notificationStream)
	if err != nil {
		return 0, fmt.Errorf("read the stream %s: %w", notificationStream, err)
	}
	return stream.CachedInfo().State.LastSeq, nil
}

// wakeOnStream is one stored message of the shared wake subject as STREAM.MSG.GET answers it.
type wakeOnStream struct {
	Subject  string `json:"subject"`
	Sequence uint64 `json:"seq"`
	Data     []byte `json:"data"`
}

// storedWake is the first message stored on the shared wake subject at or after seq, read by
// sequence (STREAM.MSG.GET with next_by_subj), which the listener's grant holds and which creates
// no consumer; found is false when none is left. It asks the API subject itself, never a direct
// get, which a grant holding MSG.GET alone refuses.
func (r *CutoverReader) storedWake(ctx context.Context, seq uint64) (wakeOnStream, bool, error) {
	request, err := json.Marshal(struct {
		Seq     uint64 `json:"seq"`
		NextFor string `json:"next_by_subj"`
	}{seq, legionWakeSubject})
	if err != nil {
		return wakeOnStream{}, false, err
	}
	ctx, cancel := context.WithTimeout(ctx, cutoverReadBound)
	defer cancel()
	reply, err := r.js.Conn().RequestWithContext(ctx, "$JS.API.STREAM.MSG.GET."+notificationStream, request)
	if err != nil {
		return wakeOnStream{}, false, err
	}
	var answer struct {
		Message *wakeOnStream `json:"message"`
		Error   *struct {
			Code        int    `json:"code"`
			ErrCode     int    `json:"err_code"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil {
		return wakeOnStream{}, false, fmt.Errorf("the answer is no JSON: %w", err)
	}
	if answer.Error != nil {
		if answer.Error.ErrCode == 10037 {
			return wakeOnStream{}, false, nil
		}
		return wakeOnStream{}, false, fmt.Errorf("the server answered %d (%d): %s", answer.Error.Code, answer.Error.ErrCode, answer.Error.Description)
	}
	if answer.Message == nil {
		return wakeOnStream{}, false, errors.New("the answer holds no message")
	}
	return *answer.Message, true, nil
}

// wakeKindOf is a stored wake envelope's report kind.
func wakeKindOf(data []byte) (string, error) {
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", fmt.Errorf("read the envelope: %w", err)
	}
	var report struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(envelope.Payload), &report); err != nil {
		return "", fmt.Errorf("read the report: %w", err)
	}
	if report.Kind == "" {
		return "", errors.New("the report names no kind")
	}
	return report.Kind, nil
}

// daemonFilters is the filter set deploy 4 leaves each of a project's daemon durables on.
func daemonFilters(project string) (map[string][]string, error) {
	own, err := wakeSubject(project)
	if err != nil {
		return nil, err
	}
	return map[string][]string{
		dispatchConsumerName(project): {dispatchIssueFilter(project)},
		githubConsumerName(project):   {githubFilter},
		wakeConsumerName(project):     {own},
		readyConsumerName(project):    {legionWakeSubject},
	}, nil
}

// durableExists is the filter read's control check (filterCheck): it holds a durable to its name
// alone, as WithinProjects holds a daemon to its consumers.
func durableExists(jetstream.ConsumerInfo, []string) bool { return true }

// filtersMatch is the filter read's check: the durable has exactly its kind's filter set.
func filtersMatch(info jetstream.ConsumerInfo, want []string) bool {
	return sameSet(filtersOf(info), want)
}

// Filters is plan C's filter read, after deploy 4 is live: every project named, and every project
// of a daemon durable on the stream, must hold its four durables, each on exactly its kind's filter
// set (daemonFilters): none on notifications.dispatch.issue.> or the mapping subject, and no wake
// durable still on the shared subject. A project prints FILTERED, GATE SHUT naming each durable and
// its filters, or UNKNOWN naming a durable it does not hold or a read it could not make.
func (r *CutoverReader) Filters(ctx context.Context, projects []string) CutoverReading {
	return r.filters(ctx, projects, filtersMatch)
}

func (r *CutoverReader) filters(ctx context.Context, projects []string, check func(jetstream.ConsumerInfo, []string) bool) CutoverReading {
	if len(projects) == 0 {
		return CutoverReading{Lines: []string{"UNKNOWN: no project named: --projects is empty"}, Verdict: GateUnknown}
	}
	consumers, err := r.consumers(ctx)
	if err != nil {
		return CutoverReading{Lines: []string{"UNKNOWN: " + err.Error()}, Verdict: GateUnknown}
	}
	var reading CutoverReading
	var shut, unknown int
	judged := projectsToJudge(projects, consumers, "dispatch", "github", "wake", "ready")
	for _, project := range judged {
		want, err := daemonFilters(project)
		if err != nil {
			reading.Lines = append(reading.Lines, fmt.Sprintf("UNKNOWN %s: %v", project, err))
			unknown++
			continue
		}
		names := slices.Sorted(func(yield func(string) bool) {
			for name := range want {
				if !yield(name) {
					return
				}
			}
		})
		var wrong, missing []string
		for _, name := range names {
			info, found := consumers[name]
			if !found {
				missing = append(missing, name)
				continue
			}
			if !check(info, want[name]) {
				wrong = append(wrong, fmt.Sprintf("%s on %s, want %s", name, strings.Join(filtersOf(info), ", "), strings.Join(want[name], ", ")))
			}
		}
		switch {
		case len(wrong) > 0:
			reading.Lines = append(reading.Lines, fmt.Sprintf("GATE SHUT %s: %s", project, strings.Join(wrong, "; ")))
			shut++
		case len(missing) > 0:
			reading.Lines = append(reading.Lines, fmt.Sprintf("UNKNOWN %s: the stream holds no %s", project, strings.Join(missing, ", ")))
			unknown++
		default:
			reading.Lines = append(reading.Lines, fmt.Sprintf("FILTERED %s: its four durables each on exactly its kind's filters", project))
		}
	}
	reading.Verdict = verdictOf(shut, unknown, len(judged))
	return reading
}

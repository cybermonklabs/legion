package replay

//ci:reads docs/site/src/content/docs/legion/replay.md `packages/daemon/internal/replay/signals_test.go#TestEverySignalOfR11HasOneDefinition` reads replayPage.
//ci:reads packages/contracts/fixtures/daemon-api/state-controller-claim.json `packages/daemon/internal/replay/signals_test.go#TestTheIdleShareFromTheStateJSON` reads stateGolden.

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

func TestEverySignalOfR11HasOneDefinition(t *testing.T) {
	t.Parallel()
	inventory := []struct {
		name      string
		inputs    []string
		source    string
		eventOnly bool
	}{
		{"idle", []string{"log.jsonl"}, "replay", false},
		{"queue", []string{"queue/<owner>__<repo>.jsonl"}, "merge queue model", false},
		{"provider", []string{"signals/provider.jsonl"}, "tokens model", false},
		{"kills", []string{"launcher/log.jsonl"}, "memory model", false},
		{"approvals", []string{"events/<KEY>.jsonl", "settings/events.jsonl", "window.json#defaultAssignee"}, "replay", true},
	}
	page, err := os.ReadFile(replayPage)
	if err != nil {
		t.Error(err)
	}
	visible := visibleText(string(page))
	isFunc := func(fn any) bool {
		v := reflect.ValueOf(fn)
		return v.Kind() == reflect.Func && !v.IsNil()
	}
	byName := map[string]int{}
	for _, def := range signalDefinitions {
		byName[def.Name]++
	}
	for _, want := range inventory {
		t.Run(want.name, func(t *testing.T) {
			count := byName[want.name]
			if count > 1 {
				t.Errorf("%s has %d definitions, want one", want.name, count)
			}
			var at, next, eventOnly bool
			var inputs []string
			var source string
			for _, def := range signalDefinitions {
				if def.Name == want.name {
					at, next, eventOnly = isFunc(def.At), isFunc(def.Next), def.EventOnly
					inputs, source = def.Inputs, def.Source
					break
				}
			}
			if !at {
				t.Errorf("no definition of %s at a moment", want.name)
			}
			if want.eventOnly {
				if !eventOnly || next {
					t.Errorf("%s must declare no Next crossing because it changes only at events", want.name)
				}
			} else if !next || eventOnly {
				t.Errorf("%s has no Next crossing function", want.name)
			}
			for _, input := range want.inputs {
				if !slices.Contains(inputs, input) {
					t.Errorf("%s does not name window input %s", want.name, input)
				}
			}
			if source != want.source {
				t.Errorf("%s names source %q, want %q", want.name, source, want.source)
			}
			heading := regexp.MustCompile("(?mi)^#{2,6} Signal `?" + want.name + "`?[ \\t]*$")
			matches := heading.FindAllStringIndex(visible, -1)
			if len(matches) != 1 {
				t.Errorf("%s has %d visible Signal %s sections, want one", replayPage, len(matches), want.name)
			} else if body := strings.TrimSpace(visible[matches[0][1]:]); body == "" || strings.HasPrefix(body, "#") {
				t.Errorf("%s has an empty Signal %s section", replayPage, want.name)
			}
		})
		delete(byName, want.name)
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		t.Errorf("signal %q is not in the R11 inventory", name)
	}
}

// signalRecordOf is w's record of the signals, d being idleAfter, which must read.
func signalRecordOf(t *testing.T, w Window, idleAfter time.Duration) *SignalRecord {
	t.Helper()
	r, err := ReadSignalRecord(w, idleAfter, 0)
	if err != nil {
		t.Fatalf("ReadSignalRecord: %v", err)
	}
	return r
}

// reading is a hand-worked value of a signal at a moment: none for a moment the window holds no
// record of the signal at.
type reading struct {
	at       time.Time
	value    float64
	recorded bool
	why      string
}

// recordedAs and unrecordedAt are a reading of value and an unrecorded one.
func recordedAs(at time.Time, value float64, why string) reading {
	return reading{at: at, value: value, recorded: true, why: why}
}

func unrecordedAt(at time.Time, why string) reading {
	return reading{at: at, why: why}
}

// checkReadings fails for each reading signal, read by at from r, gives otherwise than worked out.
func checkReadings(t *testing.T, signal string, r *SignalRecord, at func(*SignalRecord, time.Time) signals.Value, readings []reading) {
	t.Helper()
	for _, want := range readings {
		got := at(r, want.at)
		if got.Recorded != want.recorded || want.recorded && got.Value != want.value {
			t.Errorf("%s at %s = %s, want %s: %s", signal, want.at.Format(time.RFC3339), describeValue(got), describeValue(signals.Value{Recorded: want.recorded,
				Value: want.value}), want.why)
		}
	}
}

// describeValue is v as a reader of a failure reads it.
func describeValue(v signals.Value) string {
	if !v.Recorded {
		return "unrecorded"
	}
	return fmt.Sprint(v.Value)
}

// guardKillText is the text of the launcher's memory guard line of a kill of process pid of role,
// as the guard writes it.
func guardKillText(pid int, role string) string {
	return fmt.Sprintf("launcher: memory guard: ended pid=%d role=%s size=9000000000 reading=9000000000 counter=1 limit=8589934592", pid, role)
}

// killsWindow is a window from 09:00 to 12:00 whose inputs reach an hour before, with a record of
// the kills over the whole reach: the launcher's lines of the memory guard of ACME-1's pod, its
// start at 09:00 and its kills at 09:40, 09:50 and 10:35, as the fixture no-crossing-due holds them.
func killsWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.boot(clock(8, 0))
	pod := "legion-acme-acme-1"
	b.window.Launcher = []LauncherLine{
		{Time: clock(9, 0), Pod: pod, Text: "launcher: memory guard: started act_percent=85 floor_percent=10 wait=5s interval=100ms limit=8589934592 reading=23052288"},
		{Time: clock(9, 40), Pod: pod, Text: guardKillText(100, "architect")},
		{Time: clock(9, 50), Pod: pod, Text: guardKillText(101, "architect")},
		{Time: clock(10, 35), Pod: pod, Text: guardKillText(102, "architect")},
	}
	b.window.Signals = map[string][]Span{SignalKills: {{From: clock(8, 0), To: clock(12, 0)}}}
	return b.build()
}

// queueLine is a line of a window's queue file: event id, of kind at at, of pull request n of
// builtRepository, as GitHub's timeline serves it (queueLineOf).
func queueLine(id, n int, kind string, at time.Time) json.RawMessage {
	return queueLineOf(builtRepository, id, n, kind, at)
}

// queueLineOf is a line of a window's queue file: event id, of kind at at, of pull request n of
// repo, as GitHub's timeline serves it, its url naming repo and the event.
func queueLineOf(repo string, id, n int, kind string, at time.Time) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"pullRequest":%d,"item":{"id":%d,"url":"https://api.github.com/repos/%s/issues/events/%d",`+
		`"event":%q,"created_at":%q,"actor":{"login":"acme-person"}}}`, n, id, repo, id, kind, at.Format(time.RFC3339)))
}

// queueWindow is a window from 09:00 to 18:00 whose inputs reach back to 01:00, with a record of
// the merge queue over the whole reach, and five pull requests of the project:
//
//   - #1 waits from 02:00 to its merge at 03:00, one hour, the merge queue removing it in the
//     same second as it merges, which ends no wait;
//   - #2 waits from 02:30 until it is dropped at 04:30, two hours, and again from 05:00 to its
//     merge at 05:30, half an hour: two waits;
//   - #3 waits from 06:00 to its merge at 10:00, four hours;
//   - #4 waits from 09:00 until it is dropped at 11:00, two hours;
//   - #5 waits from 12:00 to its merge at 15:00, three hours.
func queueWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(18, 0))
	b.reaches(8 * time.Hour)
	b.boot(clock(1, 0))
	b.window.Queue = map[string][]json.RawMessage{builtRepository: {
		queueLine(1, 1, queueAdded, clock(2, 0)),
		queueLine(2, 1, queueMerged, clock(3, 0)),
		queueLine(3, 1, queueRemoved, clock(3, 0)),
		queueLine(4, 2, queueAdded, clock(2, 30)),
		queueLine(5, 2, queueRemoved, clock(4, 30)),
		queueLine(6, 2, queueAdded, clock(5, 0)),
		queueLine(7, 2, queueMerged, clock(5, 30)),
		queueLine(8, 3, queueAdded, clock(6, 0)),
		queueLine(9, 3, queueMerged, clock(10, 0)),
		queueLine(10, 4, queueAdded, clock(9, 0)),
		queueLine(11, 4, queueRemoved, clock(11, 0)),
		queueLine(12, 5, queueAdded, clock(12, 0)),
		queueLine(13, 5, queueMerged, clock(15, 0)),
	}}
	b.window.Signals = map[string][]Span{SignalQueue: {{From: clock(1, 0), To: clock(18, 0)}}}
	return b.build()
}

// GitHub answers a renamed repository's former name with the repository's own events, so a
// window whose queue was collected under acme/old-name, the former name of acme/new-name, and
// under acme/new-name holds a file of each, every event's url naming acme/new-name and some
// events in both files. The two files give the waits of their union, each event once, as one
// file of the union does: #1, in both files, waits once, from 02:00 to 03:00; #2, entered in
// the former name's file alone and merged in the current name's alone, waits once, from 04:00 to
// 06:00, and not from 04:00 on with no end; #3, in the current name's file alone, waits from
// 07:00 until it is dropped at 08:00; and #4, in the former name's file alone, waits from 09:00
// to 10:00. It fails where the waits are built from each file apart.
func TestQueueWaitsCountEachEventOnceAcrossARenamedRepository(t *testing.T) {
	t.Parallel()
	const current, former = "acme/new-name", "acme/old-name"
	event := func(id, n int, kind string, at time.Time) json.RawMessage {
		return queueLineOf(current, id, n, kind, at)
	}
	added1, merged1 := event(11, 1, queueAdded, clock(2, 0)), event(12, 1, queueMerged, clock(3, 0))
	added2, merged2 := event(21, 2, queueAdded, clock(4, 0)), event(22, 2, queueMerged, clock(6, 0))
	added3, removed3 := event(31, 3, queueAdded, clock(7, 0)), event(32, 3, queueRemoved, clock(8, 0))
	added4, merged4 := event(41, 4, queueAdded, clock(9, 0)), event(42, 4, queueMerged, clock(10, 0))
	want := []signals.Wait{{Added: clock(2, 0), Left: clock(3, 0)}, {Added: clock(4, 0), Left: clock(6, 0)},
		{Added: clock(7, 0), Left: clock(8, 0)}, {Added: clock(9, 0), Left: clock(10, 0)}}
	describe := func(waits []signals.Wait) string {
		var spans []string
		for _, wait := range waits {
			left := "no end"
			if !wait.Left.IsZero() {
				left = stamp(wait.Left)
			}
			spans = append(spans, stamp(wait.Added)+" to "+left)
		}
		return "[" + strings.Join(spans, ", ") + "]"
	}
	for _, tc := range []struct {
		name  string
		queue map[string][]json.RawMessage
	}{
		{"one file of the union", map[string][]json.RawMessage{current: {added1, merged1, added2, merged2, added3, removed3, added4, merged4}}},
		{"a file under each name", map[string][]json.RawMessage{
			current: {added1, merged1, merged2, added3, removed3},
			former:  {added1, merged1, added2, added4, merged4},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := queueWaits(tc.queue)
			if err != nil {
				t.Fatalf("queueWaits: %v", err)
			}
			if !slices.EqualFunc(got, want, func(a, b signals.Wait) bool { return a.Added.Equal(b.Added) && a.Left.Equal(b.Left) }) {
				t.Errorf("the queue's waits are %s, want %s", describe(got), describe(want))
			}
		})
	}
}

// providerWindow is a window from 09:00 to 12:00 with the provider's record of every minute from
// 09:00 to 11:59, 3,000 uncached input tokens a minute but 4,800 at 10:00, 6,600 at 10:01 and 5,400
// at 10:02. No limit is observed before 09:30; the project's own limit is 6,000 a minute from 09:30
// to 11:29; the limit is taken away (0) from 11:30 to 11:44; and from 11:45 the limit observed is
// one model's own, 5,000. The provider refuses 2 at 09:10 and 1 at 10:01, and the project's own
// limit refuses 3 at 10:01 and 1 at 10:30. The window holds a record of the provider in each minute
// with a limit, from 09:30, as collect writes it.
func providerWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.boot(clock(8, 0))
	limit := func(n int64, scope *string) (*int64, *string, *string) {
		source := "dispatch"
		return &n, &source, scope
	}
	project, model := projectLimitScope, "acme-model-a"
	for minute := clock(9, 0); minute.Before(clock(12, 0)); minute = minute.Add(time.Minute) {
		line := ProviderMinute{Minute: minute, Uncached: 3000}
		switch {
		case minute.Before(clock(9, 30)):
		case minute.Before(clock(11, 30)):
			line.Limit, line.LimitSource, line.LimitScope = limit(6000, &project)
		case minute.Before(clock(11, 45)):
			line.Limit, line.LimitSource, line.LimitScope = limit(0, nil)
		default:
			line.Limit, line.LimitSource, line.LimitScope = limit(5000, &model)
		}
		switch {
		case minute.Equal(clock(9, 10)):
			line.Provider = 2
		case minute.Equal(clock(10, 0)):
			line.Uncached = 4800
		case minute.Equal(clock(10, 1)):
			line.Uncached, line.Own, line.Provider = 6600, 3, 1
		case minute.Equal(clock(10, 2)):
			line.Uncached = 5400
		case minute.Equal(clock(10, 30)):
			line.Own = 1
		}
		b.window.Provider = append(b.window.Provider, line)
	}
	b.window.Signals = map[string][]Span{SignalProvider: {{From: clock(9, 30), To: clock(12, 0)}}}
	return b.build()
}

// idleWindow is a window from 10:00 to 14:00 whose inputs reach back to the daemon's boot at 09:00,
// with a record of the idle share over the whole reach. ACME-1 takes a slot at 09:05, and its
// architect's turn ends at 09:30, after which it is idle. ACME-2 takes a slot at 09:10, its
// architect's turn ends at 10:00, and it turns again from 11:30 to 12:00. ACME-3 takes a slot at
// 10:20 and its architect turns to 13:00.
func idleWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(10, 0), clock(14, 0))
	b.boot(clock(9, 0))
	promotedRoot(b, clock(9, 5), "ACME-1", "a1")
	promotedRoot(b, clock(9, 10), "ACME-2", "a2")
	b.turnEnds(clock(9, 30), "ACME-1", claim.RoleArchitect)
	b.turnEnds(clock(10, 0), "ACME-2", claim.RoleArchitect)
	promotedRoot(b, clock(10, 20), "ACME-3", "a3")
	b.turnStarts(clock(11, 30), "ACME-2", claim.RoleArchitect)
	b.turnEnds(clock(12, 0), "ACME-2", claim.RoleArchitect)
	b.turnEnds(clock(13, 0), "ACME-3", claim.RoleArchitect)
	b.window.Signals = map[string][]Span{SignalIdle: {{From: clock(9, 0), To: clock(14, 0)}}}
	return b.build()
}

// The people of the approvals' window: the project's default assignee, the one it becomes at
// 11:00, and another person.
const (
	acmeOwner = "acme-owner@example.invalid"
	acmeLead  = "acme-lead@example.invalid"
	acmeOther = "acme-other@example.invalid"
)

// approvalsWindow is a window from 09:00 to 13:00 whose inputs reach back to 08:00, where
// window.json gives acmeOwner as the project's default assignee, and a settings event makes it
// acmeLead at 11:00; it holds a record of the approvals over the whole reach. Each of ACME-1 to
// ACME-5 is filed at 08:30, ACME-2 with acmeOther as its assignee and the others with none, and:
//
//   - A1, a question on ACME-1 at 09:00, reworded at 09:50, asks nobody by name;
//   - A2, a question on ACME-2 at 09:10, whose assignee becomes acmeOwner at 09:40;
//   - A3, a question on ACME-3 at 09:20, is asked of acmeOther at 09:30;
//   - A4, a request on ACME-4 at 09:30 to approve version 1 of its spec, is moved by version 2 at
//     10:00, waiting on its agent, handed back to the person at 10:30 and answered at 12:00;
//   - A5, a question on ACME-5 at 09:00, has a person's reply at 10:10, which leaves it with its
//     agent, and the agent's at 10:40, which leaves it with the person, and is declined at 12:30.
func approvalsWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.boot(clock(8, 0))
	b.window.DefaultAssignee = acmeOwner
	for i, key := range []string{"ACME-1", "ACME-2", "ACME-3", "ACME-4", "ACME-5"} {
		f := filing{Title: "Settle the question of " + key, Status: "in_progress", Rank: fmt.Sprintf("a%d", i+1)}
		if key == "ACME-2" {
			f.Assignee = acmeOther
		}
		b.file(clock(8, 30), key, f)
	}
	ask := func(at time.Time, kind, key, id string, payload map[string]any) {
		architect := b.session(key, claim.RoleArchitect)
		body := map[string]any{"id": id, "issue_key": key, "author": architect, "kind": record.AskQuestion, "state": "open", "asked_of": nil}
		maps.Copy(body, payload)
		actor := architect
		if kind == "ask.answered" || kind == "ask.resolved" || kind == "ask.asked_of" {
			actor = builtPerson
		}
		b.event(at, key, kind, actor, body)
	}
	approval := func(version, requested int) map[string]any {
		return map[string]any{"kind": record.AskApproval, "approval": map[string]any{"artifact_id": b.artifact("ACME-4"), "name": "spec.md", "version": version,
			"requested_version": requested}}
	}
	reply := func(at time.Time, key, id string, by eventActor, waitingOn string) {
		b.event(at, key, "comment.created", by, map[string]any{"id": "comment-" + id + "-" + at.Format("1504"), "ask_id": id, "ask_state": "open",
			"ask_waiting_on": waitingOn, "body": "A reply on the ask"})
	}
	ask(clock(9, 0), "ask.opened", "ACME-1", "A1", nil)
	ask(clock(9, 0), "ask.opened", "ACME-5", "A5", nil)
	ask(clock(9, 10), "ask.opened", "ACME-2", "A2", nil)
	ask(clock(9, 20), "ask.opened", "ACME-3", "A3", nil)
	ask(clock(9, 30), "ask.asked_of", "ACME-3", "A3", map[string]any{"asked_of": acmeOther, "asked_of_by": acmeOwner})
	ask(clock(9, 30), "ask.opened", "ACME-4", "A4", approval(1, 1))
	b.edit(clock(9, 40), "ACME-2", func(issue *builtIssue) { issue.Assignee = acmeOwner })
	ask(clock(9, 50), "ask.edited", "ACME-1", "A1", map[string]any{"question": "A reworded question"})
	ask(clock(10, 0), "ask.edited", "ACME-4", "A4", approval(2, 1))
	reply(clock(10, 10), "ACME-5", "A5", builtPerson, "agent")
	ask(clock(10, 30), "ask.handed_back", "ACME-4", "A4", approval(2, 2))
	reply(clock(10, 40), "ACME-5", "A5", b.session("ACME-5", claim.RoleArchitect), "human")
	answered := approval(2, 2)
	answered["state"] = "answered"
	ask(clock(12, 0), "ask.answered", "ACME-4", "A4", answered)
	ask(clock(12, 30), "ask.resolved", "ACME-5", "A5", map[string]any{"state": "resolved",
		"resolution": map[string]any{"kind": "declined", "reason": "Not mine to answer", "actor": builtPerson, "at": clock(12, 30)}})
	b.window.Settings = []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"id":9001,"seq":1,"project":%q,"type":%q,"actor":{"kind":"user","id":%q},`+
		`"created_at":%q,"payload":{"project":%q,"assignee":%q}}`, builtProject, defaultAssigneeEvent, acmeOwner, clock(11, 0).Format(time.RFC3339),
		builtProject, acmeLead))}
	b.window.Signals = map[string][]Span{SignalApprovals: {{From: clock(8, 0), To: clock(13, 0)}}}
	return b.build()
}

// Each signal of R11 is read at moments of a window worked out by hand, one of them on an edge of
// the span its record covers (spec R11, plan t30): idleWindow's idle share at d one hour, 10
// minutes and 4 hours; queueWindow's merge queue wait; providerWindow's headroom and its
// refusals; killsWindow's kills; and approvalsWindow's asks that wait on the default assignee.
// The test fails for each reading that differs from the hand-worked one, and for each moment at
// which the approvals count asks other than those worked out.
func TestEachSignalAtAMoment(t *testing.T) {
	t.Parallel()
	t.Run("idle", func(t *testing.T) {
		w := idleWindow(t)
		checkReadings(t, SignalIdle, signalRecordOf(t, w, time.Hour), idleAt, []reading{
			recordedAs(clock(10, 0), 0, "ACME-1 is idle for 30 minutes and ACME-2 for none, so neither for an hour"),
			recordedAs(clock(10, 30), 1.0/3, "ACME-1 is idle for an hour, ACME-2 for 30 minutes, and ACME-3 works"),
			recordedAs(clock(11, 0), 2.0/3, "ACME-2 is idle for an hour too"),
			recordedAs(clock(11, 30), 1.0/3, "ACME-2 works again"),
			unrecordedAt(clock(14, 0), "the record's span ends at 14:00, which it does not hold"),
		})
		checkReadings(t, SignalIdle, signalRecordOf(t, w, 10*time.Minute), idleAt, []reading{
			recordedAs(clock(13, 10), 1, "at d 10 minutes, ACME-3, idle from its turn's end at 13:00, counts beside ACME-1 and ACME-2"),
		})
		checkReadings(t, SignalIdle, signalRecordOf(t, w, 4*time.Hour), idleAt, []reading{
			recordedAs(clock(13, 30), 1.0/3, "at d 4 hours, only ACME-1, idle from 09:30, counts"),
		})
	})
	t.Run("queue", func(t *testing.T) {
		checkReadings(t, SignalQueue, signalRecordOf(t, queueWindow(t), signals.DefaultIdleAfter), queueAt, []reading{
			recordedAs(clock(1, 0), 0, "at the start of the record's span, no pull request has waited"),
			recordedAs(clock(3, 0), 1, "#1 left after an hour, and #2 has waited 30 minutes"),
			recordedAs(clock(5, 0), 1.5, "the median of #1's hour and #2's first two hours; #2 has waited nothing again"),
			recordedAs(clock(9, 0), 3, "#3 has waited 3 hours, above the median 1.25 of #2's two waits; #1's exit at 03:00 left the 6 hours"),
			recordedAs(clock(15, 0), 3, "the median of #3's 4 hours, #4's 2 and #5's 3, with none in the queue"),
			unrecordedAt(clock(18, 0), "the record's span ends at 18:00"),
		})
	})
	t.Run("provider", func(t *testing.T) {
		r := signalRecordOf(t, providerWindow(t), signals.DefaultIdleAfter)
		checkReadings(t, SignalProvider, r, providerAt, []reading{
			unrecordedAt(clock(9, 30), "the minute before, 09:29, is before the limit's first observation"),
			recordedAs(clock(9, 31), 0.5, "the first minute of the record's span, 09:30: 3,000 of 6,000"),
			recordedAs(clock(10, 1), 0.8, "the minute 10:00: 4,800 of 6,000"),
			recordedAs(clock(10, 2), 1.1, "the minute 10:01: 6,600 of 6,000, over the limit"),
			unrecordedAt(clock(11, 31), "the minute 11:30 has no limit, which was taken away"),
			unrecordedAt(clock(11, 46), "the limit of the minute 11:45 is one model's own, not the project's"),
		})
		for _, want := range []struct {
			at   time.Time
			want signals.RefusalCount
			why  string
		}{
			{clock(9, 0), signals.RefusalCount{}, "the record holds no minute before 09:00"},
			{clock(9, 11), signals.RefusalCount{Recorded: true, Provider: 2}, "the provider's 2 of the minute 09:10, with no limit of the project's in force"},
			{clock(10, 2), signals.RefusalCount{Recorded: true, Own: 3, Provider: 3}, "09:10's and 10:01's"},
			{clock(10, 11), signals.RefusalCount{Recorded: true, Own: 3, Provider: 1}, "09:10's left the hour at 10:11"},
			{clock(10, 31), signals.RefusalCount{Recorded: true, Own: 4, Provider: 1}, "10:01's and 10:30's"},
		} {
			if got := r.Refusals(want.at); got != want.want {
				t.Errorf("refusals at %s = %+v, want %+v: %s", want.at.Format(time.RFC3339), got, want.want, want.why)
			}
		}
	})
	t.Run("kills", func(t *testing.T) {
		checkReadings(t, SignalKills, signalRecordOf(t, killsWindow(t), signals.DefaultIdleAfter), killsAt, []reading{
			recordedAs(clock(9, 39), 0, "no kill yet"),
			recordedAs(clock(9, 40), 1, "a kill counts from its own moment"),
			recordedAs(clock(10, 40), 2, "09:40's kill left the hour at 10:40, and 09:50's and 10:35's count"),
			recordedAs(clock(10, 50), 1, "09:50's kill left the hour"),
			unrecordedAt(clock(12, 0), "the record's span ends at 12:00"),
		})
	})
	t.Run("approvals", func(t *testing.T) {
		w := approvalsWindow(t)
		r := signalRecordOf(t, w, signals.DefaultIdleAfter)
		for _, want := range []struct {
			at   time.Time
			asks []string
			why  string
		}{
			{clock(8, 0), nil, "at the start of the record's span no ask is open"},
			{clock(9, 35), []string{"A1", "A4", "A5"}, "A2 waits on ACME-2's assignee, acmeOther, and A3 on acmeOther, whom it was asked of"},
			{clock(9, 45), []string{"A1", "A2", "A4", "A5"}, "ACME-2's assignee is now acmeOwner, the default assignee"},
			{clock(9, 55), []string{"A1", "A2", "A4", "A5"}, "A1's rewording leaves it waiting on the person"},
			{clock(10, 15), []string{"A1", "A2"}, "A4's version moved, so it waits on its agent, and A5 waits on its agent after the person's reply"},
			{clock(10, 45), []string{"A1", "A2", "A4", "A5"}, "A4 was handed back to the person, and the agent replied on A5"},
			{clock(11, 0), []string{"A1", "A4", "A5"}, "the default assignee is acmeLead, so A2, waiting on ACME-2's assignee acmeOwner, no longer counts"},
			{clock(12, 0), []string{"A1", "A5"}, "A4 was answered"},
			{clock(12, 30), []string{"A1"}, "A5 was declined"},
		} {
			if got := r.waitingAsks(want.at); !slices.Equal(got, want.asks) {
				t.Errorf("the asks waiting on the default assignee at %s are %v, want %v: %s", want.at.Format(time.RFC3339), got, want.asks, want.why)
			}
			checkReadings(t, SignalApprovals, r, approvalsAt, []reading{recordedAs(want.at, float64(len(want.asks)), want.why)})
		}
		checkReadings(t, SignalApprovals, r, approvalsAt, []reading{unrecordedAt(clock(13, 0), "the record's span ends at 13:00")})
	})
}

// crossing is a hand-worked next crossing of a signal's limit: the moment it is asked from, the
// limit, the moment it crosses at, zero for none due, and why.
type crossing struct {
	at    time.Time
	limit float64
	next  time.Time
	why   string
}

// checkCrossings fails for each crossing next, asked of r, gives otherwise than worked out.
func checkCrossings(t *testing.T, signal string, r *SignalRecord, next func(*SignalRecord, time.Time, float64) (time.Time, bool), crossings []crossing) {
	t.Helper()
	for _, want := range crossings {
		got, due := next(r, want.at, want.limit)
		if due != !want.next.IsZero() || due && !got.Equal(want.next) {
			t.Errorf("%s's next crossing of %v after %s = %s, want %s: %s", signal, want.limit, want.at.Format(time.RFC3339), describeCrossing(got, due),
				describeCrossing(want.next, !want.next.IsZero()), want.why)
		}
	}
}

// describeCrossing is a next crossing as a reader of a failure reads it.
func describeCrossing(at time.Time, due bool) string {
	if !due {
		return "none"
	}
	return at.Format(time.RFC3339)
}

// Each signal with a next crossing gives, at moments of the windows of TestEachSignalAtAMoment
// worked out by hand, the next moment at which it would cross a limit, in each direction it can
// cross with no event, and none where no crossing is due (spec R11, plan t30). A signal binds where
// it is recorded and at or above its limit, so the end of its record is a crossing of a signal that
// binds there (assumption 47).
func TestEachSignalsNextCrossing(t *testing.T) {
	t.Parallel()
	t.Run("idle", func(t *testing.T) {
		checkCrossings(t, SignalIdle, signalRecordOf(t, idleWindow(t), time.Hour), idleNext, []crossing{
			{at: clock(10, 0), limit: 0.5, next: clock(10, 30), why: "up: ACME-1, idle from 09:30, reaches an hour at 10:30, one of the two holders"},
			{at: clock(11, 0), limit: 0.5, next: clock(14, 0), why: "down: a holder leaves idle only at an event, so only the record's end at 14:00 ends the binding"},
			{at: clock(11, 30), limit: 0.5, why: "none: ACME-1 alone of three counts, and no holder becomes idle with no event"},
		})
	})
	t.Run("queue", func(t *testing.T) {
		checkCrossings(t, SignalQueue, signalRecordOf(t, queueWindow(t), signals.DefaultIdleAfter), queueNext, []crossing{
			{at: clock(9, 0), limit: 4, next: clock(10, 0), why: "up: #3, waiting from 06:00, reaches 4 hours at 10:00"},
			{at: clock(15, 0), limit: 3, next: clock(16, 0), why: "down: #3's exit at 10:00 leaves the 6 hours at 16:00, and the median of #4's 2 hours and #5's 3 is 2.5"},
			{at: clock(15, 0), limit: 5, why: "none: no pull request waits, and an exit leaving the 6 hours lowers the median"},
			{at: clock(17, 0), limit: 2, next: clock(18, 0), why: "down: #5's 3 hours bind until the record's end at 18:00"},
		})
	})
	t.Run("provider", func(t *testing.T) {
		r := signalRecordOf(t, providerWindow(t), signals.DefaultIdleAfter)
		checkCrossings(t, SignalProvider, r, providerNext, []crossing{
			{at: clock(9, 31), limit: 0.75, next: clock(10, 1), why: "up: the minute 10:00 uses 0.8 of the limit"},
			{at: clock(10, 2), limit: 1, next: clock(10, 3), why: "down: the minute 10:02 uses 0.9"},
			{at: clock(10, 2), limit: 0.85, next: clock(10, 4), why: "down: 0.9 still binds, and the minute 10:03 uses 0.5"},
			{at: clock(11, 0), limit: 0.5, next: clock(11, 31), why: "down: the limit taken away at 11:30 binds nothing"},
			{at: clock(10, 4), limit: 2, why: "none: no minute of the record uses twice the limit"},
		})
		checkCrossings(t, "refusals", r, (*SignalRecord).NextRefusals, []crossing{
			{at: clock(10, 2), limit: 5, next: clock(10, 11), why: "down: 09:10's refusals leave the hour at 10:11, leaving 4"},
			{at: clock(10, 31), limit: 5, next: clock(11, 2), why: "down: 10:01's refusals leave the hour at 11:02, leaving 1"},
			{at: clock(10, 11), limit: 5, why: "none: 4 refusals, and a refusal comes only at an event"},
		})
	})
	t.Run("kills", func(t *testing.T) {
		checkCrossings(t, SignalKills, signalRecordOf(t, killsWindow(t), signals.DefaultIdleAfter), killsNext, []crossing{
			{at: clock(9, 50), limit: 2, next: clock(10, 40), why: "down: 09:40's kill leaves the hour at 10:40, leaving 1"},
			{at: clock(10, 35), limit: 2, next: clock(10, 50), why: "down: at 10:40 3 fall to 2, which still binds, and at 10:50 to 1"},
			{at: clock(10, 50), limit: 1, next: clock(11, 35), why: "down: 10:35's kill leaves the hour at 11:35"},
			{at: clock(9, 45), limit: 2, why: "none: 1 kill, and a kill comes only at an event"},
		})
	})
}

// stateGolden is the body of `legion state --json` that internal/daemon's composition of the state
// with the controller's claim writes (TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds):
// one slot of six in use, LEGION-208's, idle since its admission at 2126-10-06T18:02:11Z with no
// turn ended.
var stateGolden = filepath.Join("..", "..", "..", "contracts", "fixtures", "daemon-api", "state-controller-claim.json")

// The idle share read live from R6's states and times in a body of `legion state --json` is the
// share the window's lines give at that moment (spec R11). The body is the daemon's state golden
// stateGolden; the window holds the same slot: ACME-208 takes the one slot in use at
// 2126-10-06T18:02:11Z and no turn of it ends. At d 10 minutes, an hour and 4 hours, the test
// reads both at the slot's admission, a second before d has passed, when it has, and an hour later,
// and fails where they differ or where either differs from the share worked out by hand: none
// before d has passed, the whole after.
func TestTheIdleShareFromTheStateJSON(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(stateGolden)
	if err != nil {
		t.Fatal(err)
	}
	admitted := time.Date(2126, time.October, 6, 18, 2, 11, 0, time.UTC)
	from := admitted.Truncate(time.Hour)
	b := newWindowBuilder(t, from, from.Add(6*time.Hour))
	b.boot(from.Add(-time.Hour))
	b.file(admitted.Add(-time.Minute), "ACME-208", filing{Title: "Ship the ACME-208 change", Status: "todo", Rank: "a1", Labels: handedToLegion})
	b.slotTaken(admitted, "ACME-208")
	b.window.Signals = map[string][]Span{SignalIdle: {{From: from.Add(-time.Hour), To: from.Add(6 * time.Hour)}}}
	w := b.build()
	for _, after := range []time.Duration{10 * time.Minute, time.Hour, 4 * time.Hour} {
		r := signalRecordOf(t, w, after)
		for _, at := range []time.Time{admitted, admitted.Add(after - time.Second), admitted.Add(after), admitted.Add(after + time.Hour)} {
			want := 0.0
			if !at.Before(admitted.Add(after)) {
				want = 1
			}
			live, err := IdleShareOfState(body, at, after)
			if err != nil {
				t.Fatalf("IdleShareOfState: %v", err)
			}
			if lines := idleAt(r, at); !lines.Recorded || lines.Value != live || live != want {
				t.Errorf("at %s with d %s the state gives an idle share of %v and the window's lines %s, want both %v", at.Format(time.RFC3339), after,
					live, describeValue(lines), want)
			}
		}
	}
}

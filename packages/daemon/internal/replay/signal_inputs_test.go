package replay

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// signalsDir holds samples of the outputs the signals' inputs are read from, each captured once
// from the real system and neutralized; its README.md says how each file was captured and what
// was renamed.
const signalsDir = "testdata/signals"

// signalsFixture is the bytes of the file name under signalsDir.
func signalsFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(signalsDir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// guardReading is what a launcher's entry reads as: the launcher's line, and the memory guard's
// line its text is.
type guardReading struct {
	line  LauncherLine
	guard MemoryGuardLine
}

// The signals' inputs are read from other systems' output, so each reader is held to a real
// sample of it rather than to a shape this package wrote (the shape of
// TestTheTraceReadsEveryCapturedShape): the launcher's memory guard lines as Cloud Logging serves
// them (an ended, a stopped for this rise and a started line), the timeline of a pull request of
// each merge queue shape as GitHub's REST API serves it (a queue whose merge and removal come in
// the same second, a removal with no merge, and a removal and a re-add before a merge), the
// project's settings events and the pull request attribution events as Dispatch serves them, and
// the provider's minutes as the deployment's record writes them (one minute's uncached tokens
// below zero). Each file is read with its reader and the reading held to the one written out by
// hand from the file. It fails for a file a reader refuses or misreads, for a file under
// signalsDir this test holds no reading of, and for a reading of a file that is not there.
func TestEachSignalInputReaderReadsItsCapturedShape(t *testing.T) {
	ended := MemoryGuardLine{Kind: "ended", Fields: map[string]string{"pid": "56126", "role": "implementer", "size": "2315661312",
		"reading": "7305846784", "counter": "8205848576", "shmem": "466944", "limit": "8589934592", "executable": "node"}}
	stopped := MemoryGuardLine{Kind: "stopped for this rise", Fields: map[string]string{"reason": "nothing to end", "reading": "7303979008",
		"endable": "0", "protected": "7429586944", "unread": "0", "unheld": "-125607936", "limit": "8589934592", "left_out": "0"}}
	started := MemoryGuardLine{Kind: "started", Fields: map[string]string{"act_percent": "85", "floor_percent": "10", "wait": "5s",
		"interval": "100ms", "limit": "8589934592", "reading": "23052288"}}
	launcher := []guardReading{
		{LauncherLine{Time: capturedTime(t, "2026-11-25T01:36:21.123091849Z"), Pod: "legion-acme-acme-1",
			Text: `launcher: memory guard: ended pid=56126 role=implementer size=2315661312 reading=7305846784 counter=8205848576 shmem=466944 limit=8589934592 executable="node"`}, ended},
		{LauncherLine{Time: capturedTime(t, "2026-11-25T01:41:29.753880822Z"), Pod: "legion-acme-acme-2",
			Text: "launcher: memory guard: stopped for this rise reason=nothing to end reading=7303979008 endable=0 protected=7429586944 unread=0 unheld=-125607936 limit=8589934592 left_out=0"}, stopped},
		{LauncherLine{Time: capturedTime(t, "2026-11-25T01:42:28.744419695Z"), Pod: "legion-acme-acme-3",
			Text: "launcher: memory guard: started act_percent=85 floor_percent=10 wait=5s interval=100ms limit=8589934592 reading=23052288"}, started},
	}

	queue := func(pr int, id int64, event, at, actor, commit string) QueueEvent {
		return QueueEvent{PullRequest: pr, Event: event, ID: id, Repo: "cybermonklabs/legion", At: capturedTime(t, at), Actor: actor, Commit: commit}
	}
	const agent, mergeQueue = "person-agent[bot]", "github-merge-queue[bot]"
	timelines := map[string][]QueueEvent{
		"github/timeline-5150.json": {
			queue(5150, 36963393320, "added_to_merge_queue", "2026-11-23T02:27:46Z", agent, ""),
			queue(5150, 37043365149, "merged", "2026-11-23T18:25:55Z", agent, "df18d9beddb21e3a2755887f68a13318f68b4659"),
			queue(5150, 37043365837, "removed_from_merge_queue", "2026-11-23T18:25:55Z", mergeQueue, ""),
		},
		"github/timeline-4922.json": {
			queue(4922, 36963821391, "added_to_merge_queue", "2026-11-23T02:36:07Z", agent, ""),
			queue(4922, 37040372393, "removed_from_merge_queue", "2026-11-23T17:55:05Z", agent, ""),
		},
		"github/timeline-4910.json": {
			queue(4910, 36943955399, "added_to_merge_queue", "2026-11-22T19:51:26Z", agent, ""),
			queue(4910, 36960903624, "removed_from_merge_queue", "2026-11-23T01:39:21Z", agent, ""),
			queue(4910, 36960905107, "added_to_merge_queue", "2026-11-23T01:39:23Z", agent, ""),
			queue(4910, 36964362297, "removed_from_merge_queue", "2026-11-23T02:46:55Z", mergeQueue, ""),
			queue(4910, 36964364704, "merged", "2026-11-23T02:46:58Z", agent, "08af61f72c1e54f0b07901bbc343a5571d66a68d"),
		},
	}

	person := dispatch.Actor{Kind: "user", ID: "person-1@example.invalid"}
	settings := []SettingsEvent{
		{ID: 217024, Seq: 1380, Project: "ACME", Type: "settings.architecture_source.updated", Actor: person,
			At: capturedTime(t, "2026-11-05T22:32:34.687384Z")},
		{ID: 563443, Seq: 4225, Project: "ACME", Type: "settings.project_default_assignee.updated", Actor: person,
			At: capturedTime(t, "2026-11-15T07:01:29.499724Z"), DefaultAssignee: "person-1@example.invalid"},
		{ID: 767167, Seq: 5351, Project: "ACME", Type: "settings.repo_project.updated",
			Actor: dispatch.Actor{Kind: "session", ID: "00000000-0000-4000-8000-000000000003"}, At: capturedTime(t, "2026-11-19T03:20:40.956454Z")},
	}

	// Each attribution event's issue and time, and the attribution it carries.
	attributions := []struct {
		key, at     string
		attribution DispatchAttribution
	}{
		{"ACME-1", "2026-11-24T11:12:07.653247Z", DispatchAttribution{Repo: "cybermonklabs/legion", Number: 5293, To: "ACME-1", Source: "branch"}},
		{"ACME-2", "2026-11-24T11:13:47.507781Z", DispatchAttribution{Repo: "cybermonklabs/legion", Number: 5330, To: "ACME-2", Source: "external_link"}},
	}

	limit := func(n int64) *int64 { return &n }
	text := func(s string) *string { return &s }
	minute := func(at string, uncached int64, limitIn *int64, source, scope *string, own, provider int64) ProviderMinute {
		return ProviderMinute{Minute: capturedTime(t, at), Uncached: uncached, Limit: limitIn, LimitSource: source, LimitScope: scope, Own: own, Provider: provider}
	}
	opus, claude, gatewayLog, recorded := text("claude-opus-5-5"), text("Claude"), text("gateway-log"), text("dispatch")
	minutes := []ProviderMinute{
		minute("2026-11-16T14:30:00Z", 2825036, limit(5500000), gatewayLog, opus, 23, 0),
		minute("2026-11-16T14:31:00Z", 2408497, limit(5500000), gatewayLog, opus, 0, 0),
		minute("2026-11-16T14:32:00Z", 4331774, limit(5500000), gatewayLog, opus, 16, 0),
		minute("2026-11-17T17:46:00Z", 2716348, limit(0), recorded, nil, 0, 0),
		minute("2026-11-17T17:47:00Z", 9908111, limit(0), recorded, nil, 0, 0),
		minute("2026-11-17T17:48:00Z", -3935516, limit(0), recorded, nil, 0, 0),
		minute("2026-11-17T17:49:00Z", 2497361, limit(0), recorded, nil, 0, 0),
		minute("2026-11-23T16:28:00Z", 5089823, limit(6000000), gatewayLog, claude, 0, 70),
		minute("2026-11-23T16:29:00Z", 7979594, limit(6000000), gatewayLog, claude, 14, 55),
		minute("2026-11-23T16:30:00Z", 5224805, limit(6000000), gatewayLog, claude, 127, 53),
	}

	read := []string{"launcher/memory-guard.json", "dispatch/settings-events.json", "dispatch/attribution-events.json", "provider/minutes.jsonl"}
	read = append(read, slices.Collect(maps.Keys(timelines))...)
	var captured []string
	err := filepath.WalkDir(signalsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(signalsDir, path)
		if name = filepath.ToSlash(name); name != "README.md" {
			captured = append(captured, name)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range captured {
		if !slices.Contains(read, name) {
			t.Errorf("%s/%s is captured, and this test holds no reading of it", signalsDir, name)
		}
	}
	for _, name := range read {
		if !slices.Contains(captured, name) {
			t.Errorf("this test holds a reading of %s/%s, which is not captured", signalsDir, name)
		}
	}

	t.Run("launcher/memory-guard.json", func(t *testing.T) {
		var entries []json.RawMessage
		if err := json.Unmarshal(signalsFixture(t, "launcher/memory-guard.json"), &entries); err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(launcher) {
			t.Fatalf("the file holds %d entries, want %d", len(entries), len(launcher))
		}
		for i, entry := range entries {
			line, err := ParseLauncherEntry(entry)
			if err != nil {
				t.Fatalf("ParseLauncherEntry refuses entry %d: %v", i+1, err)
			}
			if !sameReading(line, launcher[i].line) {
				t.Errorf("entry %d reads as %s, want %s", i+1, describeReading(line), describeReading(launcher[i].line))
			}
			guard, ok, err := ParseMemoryGuard(line.Text)
			if err != nil || !ok {
				t.Fatalf("ParseMemoryGuard reads entry %d's text as %v, %v; want a memory guard line", i+1, ok, err)
			}
			if !sameReading(guard, launcher[i].guard) || !maps.Equal(guard.Fields, launcher[i].guard.Fields) {
				t.Errorf("entry %d's text reads as %s, want %s", i+1, describeReading(guard), describeReading(launcher[i].guard))
			}
			if pid, ok := guard.PID(); guard.Kill() != (i == 0) || (i == 0 && (!ok || pid != 56126 || guard.Role() != "implementer")) {
				t.Errorf("entry %d is a kill: %v, of pid %d (%v) in role %q; want only the ended line one, of pid 56126 in role implementer",
					i+1, guard.Kill(), pid, ok, guard.Role())
			}
		}
	})
	for _, name := range slices.Sorted(maps.Keys(timelines)) {
		want := timelines[name]
		t.Run(name, func(t *testing.T) {
			page := signalsFixture(t, name)
			lines, err := ParseQueueTimeline(want[0].PullRequest, page)
			if err != nil {
				t.Fatalf("ParseQueueTimeline refuses the page: %v", err)
			}
			var items []json.RawMessage
			if err := json.Unmarshal(page, &items); err != nil {
				t.Fatal(err)
			}
			var kept []json.RawMessage
			for _, item := range items {
				var head struct {
					Event string `json:"event"`
				}
				if err := json.Unmarshal(item, &head); err != nil {
					t.Fatal(err)
				}
				if head.Event == "added_to_merge_queue" || head.Event == "removed_from_merge_queue" || head.Event == "merged" {
					kept = append(kept, compactJSON(t, item))
				}
			}
			if len(lines) != len(want) || len(kept) != len(want) {
				t.Fatalf("the page gives %d queue lines and holds %d queue events, want %d", len(lines), len(kept), len(want))
			}
			for i, line := range lines {
				var wrapped struct {
					PullRequest int             `json:"pullRequest"`
					Item        json.RawMessage `json:"item"`
				}
				if err := json.Unmarshal(line, &wrapped); err != nil {
					t.Fatalf("queue line %d is no line of the window's queue form: %v", i+1, err)
				}
				if wrapped.PullRequest != want[i].PullRequest || !bytes.Equal(compactJSON(t, wrapped.Item), kept[i]) {
					t.Errorf("queue line %d holds pull request %d and the item\n  %s\nwant %d and the timeline's own item\n  %s", i+1,
						wrapped.PullRequest, wrapped.Item, want[i].PullRequest, kept[i])
				}
				got, err := ParseQueueLine(line)
				if err != nil {
					t.Fatalf("ParseQueueLine refuses queue line %d: %v", i+1, err)
				}
				if !sameReading(got, want[i]) {
					t.Errorf("queue line %d reads as %s, want %s", i+1, describeReading(got), describeReading(want[i]))
				}
			}
		})
	}
	t.Run("dispatch/settings-events.json", func(t *testing.T) {
		var events []json.RawMessage
		if err := json.Unmarshal(signalsFixture(t, "dispatch/settings-events.json"), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != len(settings) {
			t.Fatalf("the file holds %d events, want %d", len(events), len(settings))
		}
		for i, event := range events {
			got, err := ParseSettingsEvent(event)
			if err != nil {
				t.Fatalf("ParseSettingsEvent refuses event %d: %v", i+1, err)
			}
			if !sameReading(got, settings[i]) {
				t.Errorf("event %d reads as %s, want %s", i+1, describeReading(got), describeReading(settings[i]))
			}
		}
	})
	t.Run("dispatch/attribution-events.json", func(t *testing.T) {
		var events []json.RawMessage
		if err := json.Unmarshal(signalsFixture(t, "dispatch/attribution-events.json"), &events); err != nil {
			t.Fatal(err)
		}
		if len(events) != len(attributions) {
			t.Fatalf("the file holds %d events, want %d", len(events), len(attributions))
		}
		for i, event := range events {
			got, err := ParseDispatchEvent(event)
			if err != nil {
				t.Fatalf("ParseDispatchEvent refuses event %d: %v", i+1, err)
			}
			want := attributions[i]
			if got.Key != want.key || !got.At.Equal(capturedTime(t, want.at)) || got.Attribution == nil || *got.Attribution != want.attribution {
				t.Errorf("event %d reads as of %s at %s with the attribution %s; want of %s at %s with %s", i+1, got.Key, stamp(got.At),
					describeReading(got.Attribution), want.key, want.at, describeReading(want.attribution))
			}
		}
	})
	t.Run("provider/minutes.jsonl", func(t *testing.T) {
		lines := bytes.Split(bytes.TrimSuffix(signalsFixture(t, "provider/minutes.jsonl"), []byte("\n")), []byte("\n"))
		if len(lines) != len(minutes) {
			t.Fatalf("the file holds %d lines, want %d", len(lines), len(minutes))
		}
		for i, line := range lines {
			got, err := ParseProviderMinute(line)
			if err != nil {
				t.Fatalf("ParseProviderMinute refuses line %d: %v", i+1, err)
			}
			if !sameReading(got, minutes[i]) {
				t.Errorf("line %d reads as %s, want %s", i+1, describeReading(got), describeReading(minutes[i]))
			}
		}
	})
}

// A queue line is read by its event's id and by the repository its event's url names, so a line
// with no id, or whose url names no repository, is refused, the refusal saying which: GitHub gives
// each timeline event its id and the url <API>/repos/<owner>/<name>/issues/events/<id>. A line
// with both is read, the API at another host and path too.
func TestParseQueueLineRefusesALineWithNoIDOrRepository(t *testing.T) {
	line := func(item string) []byte {
		return []byte(`{"pullRequest":7,"item":{` + item + `"event":"added_to_merge_queue","created_at":"2001-03-05T09:40:00Z",` +
			`"actor":{"login":"acme-person"}}}`)
	}
	const url = `"url":"https://api.github.com/repos/acme/new-name/issues/events/77",`
	for _, tc := range []struct {
		name, item, refusal string
	}{
		{"an event with its id and url", `"id":77,` + url, ""},
		{"an event of an API at another host and path", `"id":77,"url":"https://github.example.invalid/api/v3/repos/acme/new-name/issues/events/77",`, ""},
		{"no id", url, "holds no event id"},
		{"an id of null", `"id":null,` + url, "holds no event id"},
		{"an id of zero", `"id":0,` + url, "holds no event id"},
		{"no url", `"id":77,`, "names no repository"},
		{"a url of null", `"id":77,"url":null,`, "names no repository"},
		{"the url of a user", `"id":77,"url":"https://api.github.com/users/acme-person",`, "names no repository"},
		{"the url of an issue", `"id":77,"url":"https://api.github.com/repos/acme/new-name/issues/7",`, "names no repository"},
		{"a link a neutralization numbered", `"id":77,"url":"https://example.invalid/link-1",`, "names no repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseQueueLine(line(tc.item))
			switch {
			case tc.refusal == "" && err != nil:
				t.Errorf("ParseQueueLine refuses the line: %v", err)
			case tc.refusal != "" && (err == nil || !strings.Contains(err.Error(), tc.refusal)):
				t.Errorf("ParseQueueLine of the line = %v, want a refusal that says the line %s", err, tc.refusal)
			}
		})
	}
}

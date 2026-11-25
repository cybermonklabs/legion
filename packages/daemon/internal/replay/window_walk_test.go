package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// inputsTimelines is the timeline GitHub serves of each pull request of cybermonklabs/legion that
// inputsWindow's issues name, one page each: #12 is queued and merged, with a comment between, and
// #13 is queued and taken out of the queue.
var inputsTimelines = map[int][][]byte{
	12: {[]byte(`[{"id":1,"url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/1","event":"added_to_merge_queue",` +
		`"actor":{"login":"person-agent[bot]"},"commit_id":null,"created_at":"2001-03-05T09:40:00Z"},` +
		`{"id":2,"event":"commented","actor":{"login":"person-1"},"created_at":"2001-03-05T09:41:00Z"},` +
		`{"id":3,"url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/3","event":"merged","actor":{"login":"person-agent[bot]"},` +
		`"commit_id":"0123456789abcdef0123456789abcdef01234567","created_at":"2001-03-05T09:50:00Z"}]`)},
	13: {[]byte(`[{"id":4,"url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/4","event":"added_to_merge_queue",` +
		`"actor":{"login":"person-agent[bot]"},"commit_id":null,"created_at":"2001-03-05T10:00:00Z"},` +
		`{"id":5,"url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/5","event":"removed_from_merge_queue",` +
		`"actor":{"login":"person-agent[bot]"},"commit_id":null,"created_at":"2001-03-05T10:05:00Z"}]`)},
}

// inputsEvent is an issue event of ACME's issue key, as Dispatch's issue event log serves it.
func inputsEvent(id, seq int, key, kind, at, payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":%q,"artifact_id":null,"project":"ACME","seq":%d,"type":%q,`+
		`"actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,"created_at":%q,"payload":%s}`, id, key, seq, kind, at, payload))
}

// inputsAttribution is the issue.pull_request_attribution_changed event of key, numbered id and
// seq, that moves the pull request number of repo to key.
func inputsAttribution(id, seq int, key, repo string, number int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issue_key":%q,"artifact_id":null,"project":"ACME","seq":%d,"type":"issue.pull_request_attribution_changed",`+
		`"actor":{"kind":"system","id":"delivery-attribution"},"notify":false,"created_at":"2001-03-05T08:20:00Z",`+
		`"payload":{"to":%q,"url":"https://github.com/%s/pull/%d","from":null,"repo":%q,"number":%d,"source":"branch"}}`, id, key, seq, key, repo, number, repo, number))
}

// inputsHistory is acme/widgets's main as githubListing serves it to collect-merges over
// [walkFrom, walkTo), newest first: its tip, committed inside the span, and its first parent,
// committed the day before, where the walk ends.
func inputsHistory(t *testing.T) []stubCommit {
	t.Helper()
	return []stubCommit{stubbed(t, 0x721, "2026-11-20T03:00:00Z", 721, 0x720), stubbed(t, 0x720, "2026-11-19T12:00:00Z", 720, 0x71f)}
}

// inputsWindow is signalWindow with a run-out of 30 minutes and every other input a window holds:
// a log whose lines name cybermonklabs/legion#12 for ACME-1 and #14 for ACME-3; the events of
// ACME-1, filed in cybermonklabs/legion, of ACME-2, to which an attribution moves
// cybermonklabs/legion#13, and of ACME-3, filed with no repository; the merges of acme/widgets on
// release, which collect-merges of main keeps, and on main, which it replaces, and of acme/other;
// and beside the queue of acme/widgets a queue of cybermonklabs/legion, which collect-queue of
// that repository replaces.
func inputsWindow(t *testing.T) Window {
	t.Helper()
	w := signalWindow(t)
	w.After = Duration(30 * time.Minute)
	w.Log = []json.RawMessage{
		json.RawMessage(`{"time":"2001-03-05T09:10:00Z","level":"INFO","msg":"admission: slot taken","issue":"ACME-1","slots":1,"cap":2}`),
		json.RawMessage(`{"time":"2001-03-05T09:45:00Z","level":"INFO","msg":"workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once",` +
			`"issue":"ACME-1","pullRequest":12,"head":"0123456789abcdef0123456789abcdef01234567"}`),
		json.RawMessage(`{"time":"2001-03-05T10:15:00Z","level":"INFO","msg":"workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once",` +
			`"issue":"ACME-3","pullRequest":14,"head":"89abcdef0123456789abcdef0123456789abcdef"}`),
	}
	w.Events = map[string][]json.RawMessage{
		"ACME-1": {inputsEvent(1, 1, "ACME-1", "issue.created", "2001-03-05T08:05:00Z",
			`{"key":"ACME-1","status":"todo","title":"a change","repo":"cybermonklabs/legion","changed":["status"]}`)},
		"ACME-2": {
			inputsEvent(2, 1, "ACME-2", "issue.created", "2001-03-05T08:10:00Z", `{"key":"ACME-2","status":"todo","title":"another change","changed":["status"]}`),
			inputsAttribution(4, 2, "ACME-2", "cybermonklabs/legion", 13),
		},
		"ACME-3": {inputsEvent(3, 1, "ACME-3", "issue.created", "2001-03-05T08:15:00Z", `{"key":"ACME-3","status":"todo","title":"a third change","changed":["status"]}`)},
	}
	w.Merges = map[string][]Merge{
		"acme/widgets": {
			{SHA: stubSHA(0x700), At: capturedTime(t, "2026-11-20T01:00:00Z"), Branch: "release", PR: 700},
			{SHA: stubSHA(0x701), At: capturedTime(t, "2026-11-20T02:00:00Z"), Branch: "main", PR: 701},
		},
		"acme/other": {{SHA: stubSHA(0x710), At: capturedTime(t, "2026-11-20T01:30:00Z"), Branch: "main"}},
	}
	w.Queue["cybermonklabs/legion"] = []json.RawMessage{json.RawMessage(`{"pullRequest":99,"item":{"id":9,` +
		`"url":"https://api.github.com/repos/cybermonklabs/legion/issues/events/9","event":"added_to_merge_queue",` +
		`"actor":{"login":"person-agent[bot]"},"commit_id":null,"created_at":"2001-03-05T08:30:00Z"}}`)}
	return w
}

// noWait is a wait between reads of GitHub that waits for nothing.
func noWait(context.Context, time.Duration) error { return nil }

// collectInputsQueue runs collect-queue of cybermonklabs/legion into the window in dir against
// timelineStub serving inputsTimelines, and is the requests the stub served and the error it
// answered. It fails t where collect-queue reads other pull requests than #12 and #13, or names
// other pull requests of no repository than #14.
func collectInputsQueue(t *testing.T, dir string) ([]stubRequest, error) {
	t.Helper()
	server, seen := timelineStub(t, inputsTimelines)
	queued, err := CollectQueue(context.Background(), QueueConfig{Repo: "cybermonklabs/legion", Token: stubGitHubToken, API: server.URL, Window: dir, Wait: noWait})
	if err == nil && (!slices.Equal(queued.PullRequests, []int{12, 13}) || !slices.Equal(queued.NoRepo, []int{14})) {
		t.Errorf("collect-queue read the pull requests %v and names %v as of no repository; want [12 13] and [14]", queued.PullRequests, queued.NoRepo)
	}
	return seen.all(), err
}

// collectInputsMerges runs collect-merges of acme/widgets's main over [walkFrom, walkTo) into the
// window in dir against githubListing serving inputsHistory, and is the requests the stub served
// and the error it answered.
func collectInputsMerges(t *testing.T, dir string) ([]stubRequest, error) {
	t.Helper()
	server, seen := githubListing(t, inputsHistory(t))
	_, err := CollectMerges(context.Background(), MergesConfig{Repo: "acme/widgets", Branch: "main", From: walkFrom, To: walkTo,
		Token: stubGitHubToken, API: server.URL, Window: dir, Wait: noWait})
	return seen.all(), err
}

// inputsCommands is collect-queue and collect-merges as the tests of a window's inputs run them:
// each command's name, the input it writes, its run (collectInputsQueue, collectInputsMerges), and
// what it changes in the window it reads, as its code before it streamed the window did it to the
// Window it read before it wrote that Window whole (Window.Write).
var inputsCommands = []struct {
	name    string
	file    string
	collect func(t *testing.T, dir string) ([]stubRequest, error)
	change  func(t *testing.T, w *Window)
}{
	{"collect-queue", "queue/cybermonklabs__legion.jsonl", collectInputsQueue, func(t *testing.T, w *Window) {
		lines := []json.RawMessage{}
		for _, number := range []int{12, 13} {
			queued, err := ParseQueueTimeline(number, inputsTimelines[number]...)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, queued...)
		}
		w.Queue["cybermonklabs/legion"] = lines
		w.Signals[SignalQueue] = []Span{{From: w.From.Add(-time.Duration(w.Before)), To: w.End()}}
	}},
	{"collect-merges", "merges/acme__widgets.jsonl", collectInputsMerges, func(t *testing.T, w *Window) {
		kept := slices.DeleteFunc(slices.Clone(w.Merges["acme/widgets"]), func(m Merge) bool { return m.Branch == "main" })
		kept = append(kept, inputsHistory(t)[0].merge())
		slices.SortStableFunc(kept, func(a, b Merge) int { return a.At.Compare(b.At) })
		w.Merges["acme/widgets"] = kept
	}},
}

// writtenAged writes w into a directory of the test's own and sets every file of it last modified
// at the start of day, so a file written again shows a later time.
func writtenAged(t *testing.T, w Window) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "window")
	if err := w.Write(dir); err != nil {
		t.Fatal(err)
	}
	for name := range windowFiles(t, dir) {
		if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(name)), day, day); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fileState is a file's bytes and the time it was last modified.
type fileState struct {
	data     string
	modified time.Time
}

// fileStates is every file under dir, by its slash-separated path under dir, with its state.
func fileStates(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	states := make(map[string]fileState)
	for name, data := range windowFiles(t, dir) {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		states[name] = fileState{data: string(data), modified: info.ModTime()}
	}
	return states
}

// fileNames is every name a or b holds, in order.
func fileNames[V any](a, b map[string]V) []string {
	names := slices.Collect(maps.Keys(a))
	for name := range b {
		if _, held := a[name]; !held {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// changedFiles is each name whose file differs between before and after, or that only one holds,
// in order.
func changedFiles(before, after map[string]fileState) []string {
	var changed []string
	for _, name := range fileNames(before, after) {
		was, held := before[name]
		is, holds := after[name]
		if held != holds || was.data != is.data || !was.modified.Equal(is.modified) {
			changed = append(changed, name)
		}
	}
	return changed
}

// collect-queue and collect-merges write their repository's file and window.json alone: every
// other input of the window, here inputsWindow's log, events, merges of another repository, queue
// of another repository, provider record, launcher log, memory file and settings events, keeps its
// bytes and its time of modification, and the command leaves no other file. The window they leave
// is, byte for byte and as ReadWindow reads it, the window their code before it streamed the
// window left: the Window it read, with the queue of cybermonklabs/legion replaced by the timelines'
// queue events and the queue signal's record set over the reach and run-out, or with the merges of
// acme/widgets's main replaced by the listing's commit in the span, written whole by Window.Write.
// It fails where a command writes the window whole, for which every input shows a later time.
func TestCollectQueueAndMergesLeaveEveryOtherInputAsItWas(t *testing.T) {
	t.Parallel()
	for _, command := range inputsCommands {
		t.Run(command.name, func(t *testing.T) {
			dir := writtenAged(t, inputsWindow(t))
			before := fileStates(t, dir)
			want, err := ReadWindow(dir)
			if err != nil {
				t.Fatal(err)
			}
			command.change(t, &want)
			wantDir := filepath.Join(t.TempDir(), "want")
			if err := want.Write(wantDir); err != nil {
				t.Fatal(err)
			}

			if _, err := command.collect(t, dir); err != nil {
				t.Fatalf("%s: %v", command.name, err)
			}
			after := fileStates(t, dir)
			for _, name := range changedFiles(before, after) {
				if name != command.file && name != windowFile {
					t.Errorf("%s changed %s, which it does not write: before %q at %s, after %q at %s", command.name, name,
						before[name].data, before[name].modified, after[name].data, after[name].modified)
				}
			}
			if got, want := windowFiles(t, dir), windowFiles(t, wantDir); !maps.EqualFunc(got, want, bytes.Equal) {
				for _, name := range fileNames(got, want) {
					if !bytes.Equal(got[name], want[name]) {
						t.Errorf("%s left %s as\n  %q\nwant, as the window written whole,\n  %q", command.name, name, got[name], want[name])
					}
				}
			}
			read, err := ReadWindow(dir)
			if err != nil {
				t.Fatalf("ReadWindow of the window %s left: %v", command.name, err)
			}
			wantRead, err := ReadWindow(wantDir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(read, wantRead) {
				t.Errorf("the window %s left reads as\n  %+v\nwant, as the window written whole reads,\n  %+v", command.name, read, wantRead)
			}
		})
	}
}

// collect-queue and collect-merges check the whole window, as ReadWindow does, also the inputs
// they keep nothing of: a window whose memory file's sha256 is not the one window.json records,
// and a window with an events file window.json does not name, are each refused with ReadWindow's
// own refusal, before any read of GitHub and with nothing written. It fails where a command reads
// only the inputs it keeps something of.
func TestCollectQueueAndMergesRefuseWhatReadWindowRefuses(t *testing.T) {
	t.Parallel()
	for _, command := range inputsCommands {
		t.Run(command.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, spoiled, line string
			}{
				{"a memory file whose sha256 differs", memoryFile, `{"minute":"2001-03-05T08:01:00Z","pod":"legion-acme-acme-1","bytes":1000}`},
				{"an input file window.json does not name", "events/ACME-9.jsonl",
					string(inputsEvent(9, 1, "ACME-9", "issue.created", "2001-03-05T08:40:00Z", `{"key":"ACME-9","status":"todo","changed":["status"]}`))},
			} {
				t.Run(tc.name, func(t *testing.T) {
					dir := writtenAged(t, inputsWindow(t))
					file := filepath.Join(dir, filepath.FromSlash(tc.spoiled))
					data, err := os.ReadFile(file)
					if err != nil && !errors.Is(err, fs.ErrNotExist) {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, append(data, tc.line+"\n"...), 0o644); err != nil {
						t.Fatal(err)
					}
					_, refusal := ReadWindow(dir)
					if refusal == nil {
						t.Fatal("ReadWindow reads the window, want a refusal")
					}
					before := fileStates(t, dir)
					requests, err := command.collect(t, dir)
					if want := "replay: " + command.name + ": " + refusal.Error(); err == nil || err.Error() != want {
						t.Errorf("%s answered %v, want %q", command.name, err, want)
					}
					if len(requests) != 0 {
						t.Errorf("%s sent %v to GitHub, want no request", command.name, requests)
					}
					if changed := changedFiles(before, fileStates(t, dir)); len(changed) != 0 {
						t.Errorf("%s changed %v, want nothing written", command.name, changed)
					}
				})
			}
		})
	}
}

// largeSamples is how many samples largeMemoryWindow's memory file holds.
const largeSamples = 200_000

// largeMemoryWindow is a window of ACME whose memory file holds largeSamples samples, eight pods'
// a minute, beside the events of ACME-1, to which an attribution moves acme/widgets#1; and the
// size of that memory file. It holds the Window it writes no longer than it writes it.
func largeMemoryWindow(t *testing.T) (string, int64) {
	t.Helper()
	pods := make([]string, 8)
	for i := range pods {
		pods[i] = fmt.Sprintf("legion-acme-acme-%d", i+1)
	}
	w := Window{Project: "ACME", From: clock(9, 0), To: clock(11, 0), Before: Duration(time.Hour), Caps: []CapAt{{At: clock(8, 0), Cap: 2}},
		Parts: []Part{}, Lines: []string{}, Events: map[string][]json.RawMessage{"ACME-1": {inputsAttribution(1, 1, "ACME-1", "acme/widgets", 1)}},
		Memory: make([]MemorySample, 0, largeSamples)}
	for i := range largeSamples {
		w.Memory = append(w.Memory, MemorySample{Minute: day.Add(time.Duration(i/len(pods)) * time.Minute), Pod: pods[i%len(pods)], Bytes: int64(i) * 4096})
	}
	dir := filepath.Join(t.TempDir(), "window")
	if err := w.Write(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(memoryFile)))
	if err != nil {
		t.Fatal(err)
	}
	return dir, info.Size()
}

// liveHeap is the bytes the heap holds live: its allocated bytes right after a collection.
func liveHeap() int64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapAlloc)
}

// collect-queue and collect-merges hold of a window no more than they keep of it: while each reads
// GitHub, the window read, its heap holds less than a quarter of the bytes of largeMemoryWindow's
// memory file more than it held before the command began. The stub GitHub answers each read with
// an empty page, and measures the heap before it answers. It fails where a command holds the
// window's samples as it reads GitHub, as one that reads the whole window before it reads GitHub
// does.
func TestCollectQueueAndMergesHoldOneLineAtATime(t *testing.T) {
	// Serial: the test measures the process's heap (liveHeap), which the allocations of a test
	// running beside it would move, so it runs before the parallel tests start.
	dir, size := largeMemoryWindow(t)
	bound := size / 4
	for _, command := range []struct {
		name    string
		collect func(api string) error
	}{
		{"collect-queue", func(api string) error {
			_, err := CollectQueue(context.Background(), QueueConfig{Repo: "acme/widgets", Token: stubGitHubToken, API: api, Window: dir, Wait: noWait})
			return err
		}},
		{"collect-merges", func(api string) error {
			_, err := CollectMerges(context.Background(), MergesConfig{Repo: "acme/widgets", Branch: "main", From: clock(9, 0), To: clock(11, 0),
				Token: stubGitHubToken, API: api, Window: dir, Wait: noWait})
			return err
		}},
	} {
		t.Run(command.name, func(t *testing.T) {
			var mu sync.Mutex
			var base int64
			var held []int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				live := liveHeap()
				mu.Lock()
				held = append(held, live-base)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, "[]")
			}))
			defer server.Close()
			mu.Lock()
			base = liveHeap()
			mu.Unlock()
			if err := command.collect(server.URL); err != nil {
				t.Fatalf("%s: %v", command.name, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(held) == 0 {
				t.Fatalf("%s read nothing of GitHub, so nothing measured its heap", command.name)
			}
			for _, grown := range held {
				if grown >= bound {
					t.Errorf("%s held %d bytes more as it read GitHub, want under %d, a quarter of the memory file's %d", command.name, grown, bound, size)
				}
			}
		})
	}
}

// The walk of a window holds one line at a time: walked with a visitor that keeps nothing, the
// heap holds, at every 40,000th line of largeMemoryWindow's memory file, less than a quarter of the
// file's bytes more than before the walk. The bound is no test unless a walk can pass it: walked
// with a visitor that keeps every line and every sample, as ReadWindow's does, the heap holds more
// than that by the file's last line. It fails where the walk holds an input whole as it hands its
// lines to the visitor, as one that reads each file whole before it splits it does.
func TestTheWalkOfAWindowHoldsOneLineAtATime(t *testing.T) {
	// Serial: the test measures the process's heap (liveHeap), which the allocations of a test
	// running beside it would move, so it runs before the parallel tests start.
	dir, size := largeMemoryWindow(t)
	head, err := readWindowHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	bound := size / 4
	// walk is the most the heap holds more than before the walk, at every 40,000th line of the
	// memory file, walked with a visitor that keeps every line or none.
	walk := func(keep bool) int64 {
		var kept []json.RawMessage
		var samples []MemorySample
		var lines int
		var held int64
		base := liveHeap()
		err := walkWindow(dir, head, func(input windowInput) func(json.RawMessage, any) {
			if input.kind != memoryFile {
				return nil
			}
			return func(line json.RawMessage, sample any) {
				if keep {
					kept = append(kept, bytes.Clone(line))
					samples = append(samples, sample.(MemorySample))
				}
				if lines++; lines%40_000 == 0 {
					held = max(held, liveHeap()-base)
				}
			}
		})
		if err != nil {
			t.Fatalf("walkWindow: %v", err)
		}
		if lines != largeSamples {
			t.Fatalf("the walk handed %d lines of the memory file to the visitor, want %d", lines, largeSamples)
		}
		return held
	}
	if held := walk(false); held >= bound {
		t.Errorf("the walk held %d bytes more with a visitor that keeps nothing, want under %d, a quarter of the memory file's %d", held, bound, size)
	}
	if held := walk(true); held < bound {
		t.Errorf("the walk held %d bytes more with a visitor that keeps every line, under the bound %d, so the bound tests nothing", held, bound)
	}
}

// longLine is a line of a daemon's log at 09:<minute>, exactly length bytes long.
func longLine(t *testing.T, minute, length int) json.RawMessage {
	t.Helper()
	head := fmt.Sprintf(`{"time":"2001-03-05T09:%02d:00Z","level":"INFO","msg":"a long line","detail":"`, minute)
	if filler := length - len(head) - len(`"}`); filler >= 0 {
		return json.RawMessage(head + strings.Repeat("x", filler) + `"}`)
	}
	t.Fatalf("no log line is %d bytes long", length)
	return nil
}

// lineLengths is the length of each of lines.
func lineLengths(lines []json.RawMessage) []int {
	lengths := make([]int, 0, len(lines))
	for _, line := range lines {
		lengths = append(lengths, len(line))
	}
	return lengths
}

// The walk reads a line longer than it reads ahead (inputBuffer) whole: a window whose log holds,
// between short lines, a line that with its newline fills the buffer, one a byte longer, and one of
// more than three buffers, and whose events hold an event as long, reads back as written. It fails
// where the walk cuts a line at its buffer's end, or refuses a line its buffer does not hold.
func TestTheWalkReadsALineLongerThanItsBuffer(t *testing.T) {
	t.Parallel()
	short := json.RawMessage(`{"time":"2001-03-05T09:00:00Z","level":"INFO","msg":"a short line"}`)
	long := strings.Repeat("x", 3*inputBuffer+17)
	w := Window{Project: "ACME", From: clock(9, 0), To: clock(11, 0), Before: Duration(time.Hour), Caps: []CapAt{{At: clock(8, 0), Cap: 2}},
		Parts: []Part{}, Lines: []string{},
		Log: []json.RawMessage{short, longLine(t, 1, inputBuffer-1), short, longLine(t, 2, inputBuffer), longLine(t, 3, 3*inputBuffer+17), short},
		Events: map[string][]json.RawMessage{"ACME-1": {inputsEvent(1, 1, "ACME-1", "issue.created", "2001-03-05T08:05:00Z",
			`{"key":"ACME-1","status":"todo","title":"`+long+`","changed":["status"]}`)}},
	}
	dir := filepath.Join(t.TempDir(), "window")
	if err := w.Write(dir); err != nil {
		t.Fatal(err)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow: %v", err)
	}
	if !reflect.DeepEqual(read.Log, w.Log) || !reflect.DeepEqual(read.Events, w.Events) {
		t.Errorf("the log reads back as lines of %v bytes and ACME-1's events as lines of %v bytes, want %v and %v, as written",
			lineLengths(read.Log), lineLengths(read.Events["ACME-1"]), lineLengths(w.Log), lineLengths(w.Events["ACME-1"]))
	}
}

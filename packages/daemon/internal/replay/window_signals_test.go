package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// everySignal is the name of each signal of R11 a window's signals map can give a record of.
var everySignal = []string{SignalIdle, SignalQueue, SignalProvider, SignalKills, SignalApprovals}

// A window of format 1 that holds none of the signals' inputs reads as it read before they were
// added (Q4): every signal of R11 unrecorded at every moment of its reach, its span and its
// run-out, and no input of a signal, no default assignee and no P0 bound held. Each of the 17 older
// k1 windows is such a window, and TestFixtureWindows holds each to the values it gave before.
func TestAWindowWithNoSignalInputReadsEverySignalUnrecorded(t *testing.T) {
	t.Parallel()
	names := fixtureWindowNames(t)
	if len(names) != 17 {
		t.Fatalf("the older k1 windows are %d, want 17: %v", len(names), names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			w, err := ReadWindow(filepath.Join(fixtureWindowsDir, name))
			if err != nil {
				t.Fatalf("ReadWindow: %v", err)
			}
			moments := []time.Time{w.From.Add(-time.Duration(w.Before)), w.From, w.From.Add(w.To.Sub(w.From) / 2), w.To.Add(-time.Nanosecond), w.End()}
			for _, signal := range everySignal {
				for _, at := range moments {
					if w.Recorded(signal, at) {
						t.Errorf("the window holds a record of %s at %s, and it holds no input of a signal", signal, stamp(at))
					}
				}
			}
			if w.Signals != nil || w.Provider != nil || w.Launcher != nil || w.Memory != nil || w.Queue != nil || w.Settings != nil ||
				w.DefaultAssignee != "" || w.P0Bounds != nil {
				t.Errorf("the window reads with signals %v, %d provider minutes, %d launcher lines, %d memory samples, queues of %d "+
					"repositories, %d settings events, default assignee %q and P0 bounds %v; want none of them",
					w.Signals, len(w.Provider), len(w.Launcher), len(w.Memory), len(w.Queue), len(w.Settings), w.DefaultAssignee, w.P0Bounds)
			}
		})
	}
}

// The k1 windows of t37 that hold a signal's input read: signal-crosses-with-no-event and
// no-crossing-due hold the launcher's lines of the memory guard's kills, which the kills signal
// has a record of over their whole reach and span, and p0-at-its-bound the change of the P0 bound
// at 09:07. A span is half open, so a window holds no record of a signal at its span's end, and no
// record of a signal its signals map does not name.
func TestTheK1WindowsOfTheSignalsRead(t *testing.T) {
	t.Parallel()
	at := func(hour, minute int) time.Time { return time.Date(2001, time.March, 5, hour, minute, 0, 0, time.UTC) }
	kill := func(minute, pid int) LauncherLine {
		return LauncherLine{Time: at(9, 0).Add(time.Duration(minute) * time.Minute), Pod: "legion-acme-acme-1",
			Text: "launcher: memory guard: ended pid=" + strconv.Itoa(pid) + " role=architect size=9000000000 reading=9000000000 counter=1 limit=8589934592"}
	}
	for _, tc := range []struct {
		name     string
		launcher []LauncherLine
		kills    []Span
		bounds   []BoundAt
	}{
		{"signal-crosses-with-no-event", []LauncherLine{kill(40, 100), kill(50, 101)}, []Span{{From: at(9, 0), To: at(10, 55)}}, nil},
		{"no-crossing-due", []LauncherLine{kill(40, 100), kill(50, 101), kill(95, 102)}, []Span{{From: at(9, 0), To: at(12, 0)}}, nil},
		{"p0-at-its-bound", nil, nil, []BoundAt{{At: at(9, 7), Bound: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := ReadWindow(filepath.Join(fixtureWindowsDir, tc.name))
			if err != nil {
				t.Fatalf("ReadWindow refuses the window: %v", err)
			}
			if !sameReading(w.Launcher, tc.launcher) {
				t.Errorf("the launcher's lines read as %s, want %s", describeReading(w.Launcher), describeReading(tc.launcher))
			}
			if !sameReading(w.Signals[SignalKills], tc.kills) {
				t.Errorf("the kills signal's spans are %v, want %v", w.Signals[SignalKills], tc.kills)
			}
			if !sameReading(w.P0Bounds, tc.bounds) {
				t.Errorf("the P0 bounds read as %+v, want %+v", w.P0Bounds, tc.bounds)
			}
			for _, span := range tc.kills {
				if !w.Recorded(SignalKills, span.From) || !w.Recorded(SignalKills, span.To.Add(-time.Nanosecond)) || w.Recorded(SignalKills, span.To) {
					t.Errorf("the kills signal is recorded at %s, %s and %s as %v, %v and %v; want true, true and false",
						stamp(span.From), stamp(span.To.Add(-time.Nanosecond)), stamp(span.To), w.Recorded(SignalKills, span.From),
						w.Recorded(SignalKills, span.To.Add(-time.Nanosecond)), w.Recorded(SignalKills, span.To))
				}
			}
			for _, signal := range everySignal {
				if signal != SignalKills && w.Recorded(signal, w.From) {
					t.Errorf("the window holds a record of %s, which its signals map does not name", signal)
				}
			}
		})
	}
}

// signalWindow is a window holding every input of the signals and every field they add: the
// provider's minutes (one with no limit in force and one whose uncached tokens are below zero, as
// the record can give them), the launcher's lines, the pods' memory, the merge queue's events of
// one repository, the project's settings events, the default assignee, a change of the P0 bound,
// and a span of each signal.
func signalWindow(t *testing.T) Window {
	t.Helper()
	at := func(hour, minute int) time.Time { return time.Date(2001, time.March, 5, hour, minute, 0, 0, time.UTC) }
	limit, source, scope := int64(6000000), "gateway-log", "Claude"
	settings := json.RawMessage(`{"id":7,"issue_key":null,"artifact_id":null,"project":"ACME","seq":2,` +
		`"type":"settings.project_default_assignee.updated","actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,` +
		`"created_at":"2001-03-05T09:30:00Z","payload":{"project":"ACME","assignee":"person-2@example.invalid"}}`)
	queued := json.RawMessage(`{"pullRequest":12,"item":{"id":1,"url":"https://api.github.com/repos/acme/widgets/issues/events/1","event":"added_to_merge_queue",` +
		`"actor":{"login":"person-agent[bot]"},"commit_id":null,"created_at":"2001-03-05T09:40:00Z"}}`)
	return Window{
		Project: "ACME", From: at(9, 0), To: at(11, 0), Before: Duration(time.Hour), Caps: []CapAt{{At: at(8, 0), Cap: 2}},
		P0Bounds: []BoundAt{{At: at(9, 7), Bound: 1}}, Parts: []Part{}, TickInterval: Duration(time.Hour), GateSteps: nil,
		Linger: Duration(2 * time.Hour), ReviewRoundCap: 3, StalledTreeNotice: Duration(6 * time.Hour), WorkerSilence: Duration(4 * time.Hour),
		WorkerBackground: Duration(4 * time.Hour), Experiments: nil, Lines: []string{}, Collector: "0123456789abcdef0123456789abcdef01234567",
		DefaultAssignee: "person-1@example.invalid",
		Signals: map[string][]Span{
			SignalIdle: {{From: at(8, 0), To: at(11, 0)}}, SignalKills: {{From: at(8, 0), To: at(11, 0)}},
			SignalProvider: {{From: at(8, 1), To: at(8, 3)}}, SignalQueue: {{From: at(8, 0), To: at(11, 0)}},
			SignalApprovals: {{From: at(8, 0), To: at(11, 0)}},
		},
		Log:    []json.RawMessage{},
		Events: map[string][]json.RawMessage{},
		Merges: map[string][]Merge{},
		Provider: []ProviderMinute{
			{Minute: at(8, 0), Uncached: 10},
			{Minute: at(8, 1), Uncached: 5000000, Limit: &limit, LimitSource: &source, LimitScope: &scope, Own: 1, Provider: 2},
			{Minute: at(8, 2), Uncached: -20, Limit: &limit, LimitSource: &source},
		},
		Launcher: []LauncherLine{{Time: at(8, 30), Pod: "legion-acme-acme-1", Text: `launcher: memory guard: ended pid=7 role=tester size=1 reading=2 counter=3 shmem=0 limit=4 executable="go"`}},
		Memory:   []MemorySample{{Minute: at(8, 0), Pod: "legion-acme-acme-1", Bytes: 1000}, {Minute: at(8, 0), Pod: "legion-acme-acme-2", Bytes: 10}},
		Queue:    map[string][]json.RawMessage{"acme/widgets": {queued}},
		Settings: []json.RawMessage{settings},
	}
}

// A window holding every input of the signals writes each into the file of its kind, names each in
// window.json's files with its sha256, and reads back as written: each input's lines, the default
// assignee, the P0 bounds and the signals' spans. An input held empty is a file with no line,
// which reads back held: a record of no line, unlike an input not held at all.
func TestTheSignalInputsWriteAndReadBack(t *testing.T) {
	t.Parallel()
	w := signalWindow(t)
	dir := filepath.Join(t.TempDir(), "window")
	if err := w.Write(dir); err != nil {
		t.Fatalf("Write: %v", err)
	}
	read, err := ReadWindow(dir)
	if err != nil {
		t.Fatalf("ReadWindow of the written window: %v", err)
	}
	wantFiles := []string{"launcher/log.jsonl", "log.jsonl", "queue/acme__widgets.jsonl", "settings/events.jsonl", "signals/memory.jsonl",
		"signals/provider.jsonl"}
	var files []string
	for name := range read.Files {
		files = append(files, name)
	}
	slices.Sort(files)
	if !slices.Equal(files, wantFiles) {
		t.Errorf("window.json names the files %v, want %v", files, wantFiles)
	}
	for _, check := range []struct {
		what      string
		got, want any
	}{
		{"the provider's minutes", read.Provider, w.Provider},
		{"the launcher's lines", read.Launcher, w.Launcher},
		{"the pods' memory", read.Memory, w.Memory},
		{"the P0 bounds", read.P0Bounds, w.P0Bounds},
	} {
		if !sameReading(check.got, check.want) {
			t.Errorf("%s read back as %s, want %s", check.what, describeReading(check.got), describeReading(check.want))
		}
	}
	if !reflect.DeepEqual(read.Queue, w.Queue) || !reflect.DeepEqual(read.Settings, w.Settings) {
		t.Errorf("the queue reads back as %s and the settings events as %s; want %s and %s", read.Queue, read.Settings, w.Queue, w.Settings)
	}
	if read.DefaultAssignee != w.DefaultAssignee || !sameSignals(read.Signals, w.Signals) {
		t.Errorf("the default assignee reads back as %q and the signals as %v; want %q and %v", read.DefaultAssignee, read.Signals,
			w.DefaultAssignee, w.Signals)
	}

	empty := signalWindow(t)
	empty.Launcher, empty.Settings = []LauncherLine{}, []json.RawMessage{}
	dir = filepath.Join(t.TempDir(), "empty")
	if err := empty.Write(dir); err != nil {
		t.Fatalf("Write of the window with empty inputs: %v", err)
	}
	if read, err = ReadWindow(dir); err != nil {
		t.Fatalf("ReadWindow of the window with empty inputs: %v", err)
	}
	if read.Launcher == nil || len(read.Launcher) != 0 || read.Settings == nil || len(read.Settings) != 0 {
		t.Errorf("the empty launcher log and settings file read back as %v and %v, want held and empty", read.Launcher, read.Settings)
	}
}

// ReadWindow refuses a window whose record of the signals no reader could trust: a signal its
// signals map names that is none of R11's, a span that is empty or that overlaps another of its
// signal, a record of the kills, the provider or the queue with no file of that input, a line of
// an input that is not that input's form, an input file of the format's directories with a name
// the format does not give, and a negative P0 bound.
func TestReadWindowRefusesABadSignalRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		header func(h map[string]any)
		files  map[string]string
		refuse string
	}{
		{"a signal of no name of R11's", func(h map[string]any) {
			h["signals"] = map[string]any{"tokens": []any{map[string]any{"from": "2001-03-05T08:00:00Z", "to": "2001-03-05T09:00:00Z"}}}
		}, nil, `record of "tokens", which is no signal of R11`},
		{"an empty span", func(h map[string]any) {
			h["signals"] = map[string]any{"idle": []any{map[string]any{"from": "2001-03-05T09:00:00Z", "to": "2001-03-05T09:00:00Z"}}}
		}, nil, "record of idle holds the empty span"},
		{"overlapping spans", func(h map[string]any) {
			h["signals"] = map[string]any{"idle": []any{map[string]any{"from": "2001-03-05T08:00:00Z", "to": "2001-03-05T09:30:00Z"},
				map[string]any{"from": "2001-03-05T09:00:00Z", "to": "2001-03-05T10:00:00Z"}}}
		}, nil, "record of idle holds spans out of order or overlapping"},
		{"kills with no launcher log", func(h map[string]any) {
			h["signals"] = map[string]any{"kills": []any{map[string]any{"from": "2001-03-05T08:00:00Z", "to": "2001-03-05T09:00:00Z"}}}
		}, nil, "record of kills, and the window holds no " + launcherFile},
		{"the provider with no provider record", func(h map[string]any) {
			h["signals"] = map[string]any{"provider": []any{map[string]any{"from": "2001-03-05T08:00:00Z", "to": "2001-03-05T09:00:00Z"}}}
		}, nil, "record of provider, and the window holds no " + providerFile},
		{"the queue with no queue file", func(h map[string]any) {
			h["signals"] = map[string]any{"queue": []any{map[string]any{"from": "2001-03-05T08:00:00Z", "to": "2001-03-05T09:00:00Z"}}}
		}, nil, "record of queue, and the window holds no queue file"},
		{"a provider line of another form", nil, map[string]string{providerFile: `{"minute":"2001-03-05T08:00:00Z","uncached":1,"limit":null,` +
			`"limit_source":null,"limit_scope":null,"own":0,"provider":0,"team":"x"}` + "\n"}, providerFile + " line 1: "},
		{"a launcher line with no pod", nil, map[string]string{launcherFile: `{"time":"2001-03-05T08:00:00Z","text":"launcher: started"}` + "\n"}, launcherFile + " line 1: "},
		{"a memory line with no minute", nil, map[string]string{memoryFile: `{"pod":"legion-acme-acme-1","bytes":5}` + "\n"}, memoryFile + " line 1: "},
		{"a queue file of no repository", nil, map[string]string{"queue/widgets.jsonl": ""}, `"queue/widgets.jsonl", which is no input`},
		{"a negative P0 bound", func(h map[string]any) {
			h["p0Bounds"] = []any{map[string]any{"at": "2001-03-05T08:00:00Z", "bound": -1}}
		}, nil, "P0 bound -1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "window")
			w := signalWindow(t)
			w.Signals, w.Provider, w.Launcher, w.Memory, w.Queue, w.Settings, w.P0Bounds = nil, nil, nil, nil, nil, nil, nil
			if err := w.Write(dir); err != nil {
				t.Fatal(err)
			}
			rewriteWindow(t, dir, tc.header, tc.files)
			// The window's directory is named after the test, so it is cut out of the refusal first.
			if _, err := ReadWindow(dir); err == nil || !strings.Contains(strings.ReplaceAll(err.Error(), dir, ""), tc.refuse) {
				t.Errorf("ReadWindow = %v, want a refusal naming %q", err, tc.refuse)
			}
		})
	}
}

// rewriteWindow changes the window in dir by hand: header changes window.json's members, and each
// of files is written with its sha256 in window.json's files, as Write would name it.
func rewriteWindow(t *testing.T, dir string, header func(map[string]any), files map[string]string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, windowFile))
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	if err := json.Unmarshal(data, &h); err != nil {
		t.Fatal(err)
	}
	if header != nil {
		header(h)
	}
	names := h["files"].(map[string]any)
	for name, content := range files {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		names[name] = hex.EncodeToString(sum[:])
	}
	if data, err = json.MarshalIndent(h, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, windowFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// sameSignals says whether two signals maps name the same signals with the same spans.
func sameSignals(a, b map[string][]Span) bool {
	if len(a) != len(b) {
		return false
	}
	for signal, spans := range a {
		if _, named := b[signal]; !named || !sameReading(spans, b[signal]) {
			return false
		}
	}
	return true
}

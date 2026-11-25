package replay

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// WindowFormat is the version of the recorded window's layout this package reads and writes,
// window.json's "format". A window of another format is refused rather than read as this one.
const WindowFormat = 1

// The files of a window, by their names relative to its directory. window.json is the window
// itself; the others hold the inputs it names in its files map.
const (
	windowFile   = "window.json"
	logFile      = "log.jsonl"
	eventsDir    = "events"
	mergesDir    = "merges"
	providerFile = "signals/provider.jsonl"
	memoryFile   = "signals/memory.jsonl"
	launcherFile = "launcher/log.jsonl"
	queueDir     = "queue"
	settingsFile = "settings/events.jsonl"
)

// The signals of R11 a window's signals map holds a record of, each by the name the map gives its
// record under: the names package signals defines. The provider's refusals ride on the provider's
// record, so the map names no record of them.
const (
	SignalIdle      = signals.Idle
	SignalQueue     = signals.Queue
	SignalProvider  = signals.Provider
	SignalKills     = signals.Kills
	SignalApprovals = signals.Approvals
)

// windowSignals is every signal a window's signals map may name.
var windowSignals = []string{SignalIdle, SignalQueue, SignalProvider, SignalKills, SignalApprovals}

// Window is one recorded window of a daemon's project: what the daemon was told and what it said
// over [From, To), read from a directory (ReadWindow) and written to one (Write). window.json
// holds every field with a JSON name; the inputs it names in Files are the files beside it, held
// here as read:
//
//   - log.jsonl, the daemon's structured lines, one JSON object per line, exactly as its JSON
//     handler writes them on stderr (time, level, msg and the attributes), in the order written;
//   - events/<KEY>.jsonl, each issue's Dispatch events, one raw event per line, oldest first,
//     exactly as Dispatch's issue event log serves them (the shape intake.DecodeDispatchEvent
//     reads);
//   - merges/<owner>__<repo>.jsonl, the first-parent commits of a repository's base branches,
//     each at GitHub's merge event where its line says so ("event"), else at its committer's
//     time (Merge);
//
// and the inputs of R11's signals, each optional:
//
//   - signals/provider.jsonl, the deployment's record of the model provider, one ProviderMinute a
//     minute;
//   - launcher/log.jsonl, the launcher's lines of the project's issue pods (LauncherLine), whose
//     memory guard lines are the record of its kills;
//   - signals/memory.jsonl, the pods' memory, one MemorySample a minute for each pod;
//   - queue/<owner>__<repo>.jsonl, the merge queue's events of the repository's pull requests,
//     each the timeline event as GitHub serves it with its pull request's number (ParseQueueLine);
//   - settings/events.jsonl, the project's own settings events of Dispatch's event log, verbatim,
//     in the log's order (ParseSettingsEvent).
//
// A window with none of the signals' inputs, as every window collected before them, holds no
// record of any signal (Recorded).
//
// A window is the record a replay runs from (RunConfig.Window) and the record a scorecard is read
// from (Score), so both read the same bytes, and the hash of each input in Files is what says
// they are the bytes collect wrote.
type Window struct {
	// Format is WindowFormat.
	Format int `json:"format"`
	// Project is the Dispatch project key of the daemon the window was recorded from.
	Project string `json:"project"`
	// From and To bound the window: the replay starts its daemon at From, and its measures read
	// [From, To).
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Before is how far before From the inputs reach, so the state at From can be rebuilt from
	// them.
	Before Duration `json:"before"`
	// After is the window's run-out: how far past To the inputs reach, so the work the record
	// shows after To runs in a replay, which goes on to the run-out's end (End) and is measured
	// over [From, To) alone. A window collected with no run-out, or before collect read one, holds
	// none, and window.json then names none.
	After Duration `json:"after,omitempty"`
	// Caps is the admission cap the daemon ran with, each from its At on.
	Caps []CapAt `json:"caps"`
	// P0Bounds is R10's bound on the slots P0 work may hold, each from its At on, as the daemon's
	// line of each change of the bound gives it; none where the window holds no such line, and
	// window.json then names none.
	P0Bounds []BoundAt `json:"p0Bounds,omitempty"`
	// Parts says which release, commit and slot rule the daemon ran over each part of the window.
	Parts []Part `json:"parts"`
	// TickInterval is the deployment's controller_wake_interval: the period of the tick passes.
	TickInterval Duration `json:"tickInterval"`
	// GateSteps is the deployment's design gate steps (record.DesignStep words); the empty list is
	// a deployment that gates none.
	GateSteps []record.DesignStep `json:"gateSteps"`
	// Linger, ReviewRoundCap, StalledTreeNotice, WorkerSilence and WorkerBackground are the
	// deployment's settings of the same names (config.Config) that change what the engine and
	// the admission pass decide, so a replay decides with the values the record was made under.
	Linger            Duration `json:"linger"`
	ReviewRoundCap    int      `json:"reviewRoundCap"`
	StalledTreeNotice Duration `json:"stalledTreeNotice"`
	WorkerSilence     Duration `json:"workerSilence"`
	WorkerBackground  Duration `json:"workerBackground"`
	// Experiments is the experiments a replay draws assignments for (admit's UseExperiments). A
	// draw records an arm for a unit and decides nothing about a slot (admit's drawExperiments,
	// experiment.DrawAndRecord), so a replay's measures are the same with or without them, and
	// collect writes none.
	Experiments []config.Experiment `json:"experiments"`
	// Lines is every msg of a log line the collector kept, so a reader of the log can tell a kind
	// the daemon did not write in the window from a kind the window does not hold.
	Lines []string `json:"lines"`
	// Collector is the commit of cybermonklabs/legion whose collect wrote the window.
	Collector string `json:"collector"`
	// Files is the sha256, as lower-case hex, of every input file, by its path relative to the
	// window's directory, written with forward slashes.
	Files map[string]string `json:"files"`
	// DefaultAssignee is the project's default assignee in Dispatch at the start of the inputs'
	// reach where no settings event before that start names it, as the deployment gave it: a
	// pod's credential cannot read the value, and the value a migration filled has no event.
	DefaultAssignee string `json:"defaultAssignee,omitempty"`
	// Signals is, for each signal of R11 the window holds a record of, the spans it holds one in,
	// in time order; a signal it names no span of has no record anywhere in the window (Q4).
	Signals map[string][]Span `json:"signals,omitempty"`
	// MissingAncestors is each issue on the Dispatch parent chain of an issue the window holds, up
	// to Dispatch's walk of 32 levels above it (ancestorLevels), whose events collect could not read,
	// since Dispatch answered that it has no such issue, as it answers for an issue it does not show
	// the reader: an input that the priority Dispatch resolves for an issue below it may rest on
	// (spec R10), sorted. None where collect read every ancestor, or where it was collected before
	// collect read ancestors, and window.json then names none.
	MissingAncestors []string `json:"missingAncestors,omitempty"`

	// Log is log.jsonl's lines, in the file's order.
	Log []json.RawMessage `json:"-"`
	// Events is each events/<KEY>.jsonl's lines, oldest first, by issue key.
	Events map[string][]json.RawMessage `json:"-"`
	// Merges is each merges file's commits, by repository as owner/repo.
	Merges map[string][]Merge `json:"-"`
	// Provider is signals/provider.jsonl's minutes, Launcher launcher/log.jsonl's lines, Memory
	// signals/memory.jsonl's samples and Settings settings/events.jsonl's events, each in its
	// file's order and nil where the window holds no such file; Queue is each queue file's lines,
	// by repository as owner/repo. An input held empty is a file with no line: a record that
	// holds nothing, which is not a missing record.
	Provider []ProviderMinute             `json:"-"`
	Launcher []LauncherLine               `json:"-"`
	Memory   []MemorySample               `json:"-"`
	Queue    map[string][]json.RawMessage `json:"-"`
	Settings []json.RawMessage            `json:"-"`
}

// Recorded says whether w holds a record of signal at at: whether at is in one of the spans its
// signals map gives that signal.
func (w Window) Recorded(signal string, at time.Time) bool {
	for _, span := range w.Signals[signal] {
		if span.Holds(at) {
			return true
		}
	}
	return false
}

// Span is the time span [From, To).
type Span struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Holds says whether at is in s.
func (s Span) Holds(at time.Time) bool {
	return !at.Before(s.From) && at.Before(s.To)
}

// BoundAt is R10's bound on the slots P0 work may hold, from At on.
type BoundAt struct {
	At    time.Time `json:"at"`
	Bound int       `json:"bound"`
}

// End is where w's inputs end: To, plus the run-out where w holds one (After). A replay runs to it,
// so the work the record shows after To runs where the replay moves it, and measures [From, To)
// alone.
func (w Window) End() time.Time {
	return w.To.Add(time.Duration(w.After))
}

// CapAt is the admission cap the daemon ran with from At on.
type CapAt struct {
	At  time.Time `json:"at"`
	Cap int       `json:"cap"`
}

// Part is one stretch of a window under one daemon: the release it ran, the commit of
// cybermonklabs/legion that release was built from, and the name of the slot rule that commit's
// daemon ran (record.SlotRuleNamed).
type Part struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Release string    `json:"release"`
	Commit  string    `json:"commit"`
	Rule    string    `json:"rule"`
}

// Merge is one first-parent commit of a repository's base branch: what the conflict draw reads
// the rate of the base's movement from, each commit at At, in the order of At and, at one At, in
// the file's order, which collect-merges writes in first-parent order. Where Event is set, At is
// GitHub's merge event, the merged_at of the pull request PR, which GitHub records as merged as
// this commit (merge_commit_sha) into Branch (base.ref). Where it is not, At is the commit's
// committer's time: an estimate of when the commit landed, and no merge event, which a reading
// that needs the moment of a merge takes for unknown. That is so of every commit GitHub records no
// such merge of, and of every commit of a window collected before collect-merges read merge
// events, whose lines name no event. PR is then 0, but in such an older window, where it is the
// pull request the first line of the commit's message names by GitHub's squash suffix ` (#<n>)`,
// 0 for none. The draw reads PR to leave a stretch's own merge out of the stretch's count
// (Stretch).
type Merge struct {
	SHA    string    `json:"sha"`
	At     time.Time `json:"at"`
	Branch string    `json:"branch"`
	PR     int       `json:"pr,omitempty"`
	Event  bool      `json:"event,omitempty"`
}

// Duration is a time.Duration written in a window as Go's duration text ("90m", "1h30m0s"), so a
// person reading window.json reads the setting as the deployment's file gives it.
type Duration time.Duration

// MarshalText writes d as time.Duration.String does.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalText reads text as time.ParseDuration does.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// readWindowHead is the window in dir as window.json alone gives it, its inputs not read: what a
// comparison needs of a window it runs no part of, and where ReadWindow begins.
func readWindowHead(dir string) (Window, error) {
	raw, err := os.ReadFile(filepath.Join(dir, windowFile))
	if err != nil {
		return Window{}, fmt.Errorf("read the window in %s: %w", dir, err)
	}
	var w Window
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w); err != nil {
		return Window{}, fmt.Errorf("read %s: %w", filepath.Join(dir, windowFile), err)
	}
	if w.Format != WindowFormat {
		return Window{}, fmt.Errorf("read the window in %s: format %d, want %d", dir, w.Format, WindowFormat)
	}
	return w, nil
}

// ReadWindow reads the window in dir: window.json (readWindowHead), then every input its Files
// names, each read once, a line at a time, and hashed as it is read (walkWindow), every line of
// every input held. It refuses a window of another format, an input that is missing or whose hash
// differs, a name in Files that is no input of the format, a line of an input that is not one JSON
// object or not of that input's form, an input file in dir that Files does not name, since nothing
// would check it, and a record of the signals no reader could trust (checkSignals). Other files of
// dir — a fixture's README.md and expect.json — are no part of the window and are not read.
func ReadWindow(dir string) (Window, error) {
	w, err := readWindowHead(dir)
	if err != nil {
		return Window{}, err
	}
	w.Events = make(map[string][]json.RawMessage)
	w.Merges = make(map[string][]Merge)
	err = walkWindow(dir, w, func(input windowInput) func(json.RawMessage, any) {
		switch input.kind {
		case logFile:
			return func(line json.RawMessage, _ any) {
				w.Log = append(w.Log, bytes.Clone(line))
			}
		case eventsDir:
			w.Events[input.key] = nil
			return func(line json.RawMessage, _ any) {
				w.Events[input.key] = append(w.Events[input.key], bytes.Clone(line))
			}
		case mergesDir:
			w.Merges[input.key] = []Merge{}
			return func(_ json.RawMessage, merge any) {
				w.Merges[input.key] = append(w.Merges[input.key], merge.(Merge))
			}
		case providerFile:
			w.Provider = []ProviderMinute{}
			return func(_ json.RawMessage, minute any) {
				w.Provider = append(w.Provider, minute.(ProviderMinute))
			}
		case launcherFile:
			w.Launcher = []LauncherLine{}
			return func(_ json.RawMessage, launched any) {
				w.Launcher = append(w.Launcher, launched.(LauncherLine))
			}
		case memoryFile:
			w.Memory = []MemorySample{}
			return func(_ json.RawMessage, sample any) {
				w.Memory = append(w.Memory, sample.(MemorySample))
			}
		case queueDir:
			if w.Queue == nil {
				w.Queue = make(map[string][]json.RawMessage)
			}
			w.Queue[input.key] = []json.RawMessage{}
			return func(line json.RawMessage, _ any) {
				w.Queue[input.key] = append(w.Queue[input.key], bytes.Clone(line))
			}
		case settingsFile:
			w.Settings = []json.RawMessage{}
			return func(line json.RawMessage, _ any) {
				w.Settings = append(w.Settings, bytes.Clone(line))
			}
		}
		return nil
	})
	if err != nil {
		return Window{}, err
	}
	return w, nil
}

// windowInput is an input file of a window as walkWindow opens it: its name in window.json's
// files, which input of the format it is and the key it holds (inputOf).
type windowInput struct {
	name, kind, key string
}

// windowVisitor is what walkWindow hands a window's inputs to. It is called as each input is
// opened, and the function it returns, where it returns one, with each line of that input in turn:
// the line, the white space around it cut off, and, for an input a Window holds decoded, the line
// decoded (decodeInputLine). The line is walkWindow's buffer, which it reads the next line into, so
// a visitor that keeps a line keeps a copy of it.
type windowVisitor func(input windowInput) func(line json.RawMessage, value any)

// inputBuffer is how many bytes of an input walkWindow reads ahead. A line longer than that is
// gathered in a buffer of its own, which grows to the longest such line (inputReader).
const inputBuffer = 64 << 10

// walkWindow reads the inputs of head, the window in dir as readWindowHead gives it, and hands each
// to visit: every input head's Files names, in the order of their names, each read once, a line at
// a time, and hashed as it is read. It holds one line at a time and nothing else of an input but
// what visit keeps. It refuses, as ReadWindow says, a name in Files that is no input of the format,
// an input that is missing or whose hash differs, a line that is not one JSON object or not of its
// input's form, an input file in dir that Files does not name, and a record of the signals no
// reader could trust (checkSignals). An input is judged once it is read whole, as a read of the
// whole file judges it: by its hash first, then by its first line that is not one JSON object,
// then by its first line not of its input's form. visit is given no line from an input's first
// refused line on, and the walk ends at the first input it refuses.
func walkWindow(dir string, head Window, visit windowVisitor) error {
	reader := inputReader{buffered: bufio.NewReaderSize(nil, inputBuffer), hash: sha256.New()}
	for _, name := range slices.Sorted(maps.Keys(head.Files)) {
		kind, key, err := inputOf(name)
		if err != nil {
			return fmt.Errorf("read the window in %s: %w", dir, err)
		}
		file, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("read the window in %s: %w", dir, err)
		}
		err = reader.walk(file, name, kind, head.Files[name], visit(windowInput{name: name, kind: kind, key: key}))
		file.Close()
		if err != nil {
			return fmt.Errorf("read the window in %s: %w", dir, err)
		}
	}
	present, err := inputFiles(dir)
	if err != nil {
		return fmt.Errorf("read the window in %s: %w", dir, err)
	}
	for _, name := range present {
		if _, listed := head.Files[name]; !listed {
			return fmt.Errorf("read the window in %s: %s is an input file window.json does not name, so nothing checks it", dir, name)
		}
	}
	if err := head.checkSignals(heldInputs(head.Files)); err != nil {
		return fmt.Errorf("read the window in %s: %w", dir, err)
	}
	return nil
}

// inputReader reads the inputs of a window one after another, each a line at a time, and hashes
// each as it reads it. It keeps its buffers from one input to the next: the bytes it reads ahead,
// and long, which holds a line longer than those.
type inputReader struct {
	buffered *bufio.Reader
	hash     hash.Hash
	long     []byte
}

// walk reads input, the input name of a window, of kind (inputOf), whose sha256 window.json
// records as sum, and hands each of its lines to visit, where visit is not nil. It refuses the
// input as walkWindow says.
func (r *inputReader) walk(input io.Reader, name, kind, sum string, visit func(json.RawMessage, any)) error {
	r.hash.Reset()
	r.buffered.Reset(io.TeeReader(input, r.hash))
	var notObject, notForm error
	for number := 1; ; number++ {
		line, ended, err := r.line()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if number == 1 && ended && len(line) == 0 {
			// A newline alone holds no line, as an empty file holds none.
			if _, err := r.buffered.Peek(1); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
		}
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("{")) || !json.Valid(line) {
			notObject = fmt.Errorf("%s: line %d is not one JSON object", name, number)
			break
		}
		if notForm != nil {
			continue
		}
		value, err := decodeInputLine(kind, line)
		switch {
		case err != nil:
			notForm = fmt.Errorf("%s line %d: %w", name, number, err)
		case visit != nil:
			visit(line, value)
		}
	}
	// What follows a line that is not one JSON object is read for the hash alone.
	if _, err := io.Copy(io.Discard, r.buffered); err != nil {
		return err
	}
	if got := hex.EncodeToString(r.hash.Sum(nil)); got != sum {
		return fmt.Errorf("the sha256 of %s is %s, and window.json records %s", name, got, sum)
	}
	if notObject != nil {
		return notObject
	}
	return notForm
}

// line is the input's next line, its newline cut off, and whether a newline ended it; io.EOF where
// the input holds no more. The line is one of r's buffers, which its next read writes over.
func (r *inputReader) line() ([]byte, bool, error) {
	line, err := r.buffered.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		r.long = append(r.long[:0], line...)
		for errors.Is(err, bufio.ErrBufferFull) {
			line, err = r.buffered.ReadSlice('\n')
			r.long = append(r.long, line...)
		}
		line = r.long
	}
	switch {
	case err == nil:
		return line[:len(line)-1], true, nil
	case errors.Is(err, io.EOF) && len(line) > 0:
		return line, false, nil
	}
	return nil, false, err
}

// decodeInputLine checks line, a line of an input of kind (inputOf), against that input's form,
// and is the line decoded for an input a Window holds decoded: a Merge, a ProviderMinute, a
// LauncherLine or a MemorySample; nil for the others, which a Window holds as their lines.
func decodeInputLine(kind string, line []byte) (any, error) {
	switch kind {
	case mergesDir:
		var merge Merge
		if err := json.Unmarshal(line, &merge); err != nil {
			return nil, err
		}
		return merge, nil
	case providerFile:
		minute, err := ParseProviderMinute(line)
		if err != nil {
			return nil, err
		}
		return minute, nil
	case launcherFile:
		var launched LauncherLine
		if err := decodeStrict(line, &launched); err != nil {
			return nil, err
		}
		if err := launched.check(); err != nil {
			return nil, err
		}
		return launched, nil
	case memoryFile:
		var sample MemorySample
		if err := decodeStrict(line, &sample); err != nil {
			return nil, err
		}
		if err := sample.check(); err != nil {
			return nil, err
		}
		return sample, nil
	case queueDir:
		_, err := ParseQueueLine(line)
		return nil, err
	case settingsFile:
		_, err := ParseSettingsEvent(line)
		return nil, err
	}
	return nil, nil
}

// heldInputs is each input of the format files names, by its kind (inputOf).
func heldInputs(files map[string]string) map[string]bool {
	held := make(map[string]bool)
	for name := range files {
		if kind, _, err := inputOf(name); err == nil {
			held[kind] = true
		}
	}
	return held
}

// checkSignals refuses a record of the signals no reader could trust: a signal the signals map
// names that is none of R11's, a span that is empty, spans of one signal out of order or
// overlapping, a record of the kills with no launcher log, of the provider with no provider
// record or of the queue with no queue file, and a P0 bound below zero. held says which inputs
// the window holds, by their kind (inputOf): launcherFile, providerFile and queueDir, a queue file
// of any repository. Its refusal names the files by the names window.json gives them.
func (w Window) checkSignals(held map[string]bool) error {
	for _, signal := range slices.Sorted(maps.Keys(w.Signals)) {
		if !slices.Contains(windowSignals, signal) {
			return fmt.Errorf("window.json names a record of %q, which is no signal of R11 (%s)", signal, strings.Join(windowSignals, ", "))
		}
		for i, span := range w.Signals[signal] {
			if !span.From.Before(span.To) {
				return fmt.Errorf("the record of %s holds the empty span [%s, %s)", signal, stamp(span.From), stamp(span.To))
			}
			if previous := w.Signals[signal][max(i-1, 0)]; i > 0 && span.From.Before(previous.To) {
				return fmt.Errorf("the record of %s holds spans out of order or overlapping: [%s, %s) then [%s, %s)", signal,
					stamp(previous.From), stamp(previous.To), stamp(span.From), stamp(span.To))
			}
		}
	}
	inputs := map[string]string{SignalKills: launcherFile, SignalProvider: providerFile, SignalQueue: queueDir}
	names := map[string]string{SignalKills: launcherFile, SignalProvider: providerFile, SignalQueue: "queue file"}
	for _, signal := range []string{SignalKills, SignalProvider, SignalQueue} {
		if len(w.Signals[signal]) > 0 && !held[inputs[signal]] {
			return fmt.Errorf("window.json names a record of %s, and the window holds no %s", signal, names[signal])
		}
	}
	for _, bound := range w.P0Bounds {
		if bound.Bound < 0 {
			return fmt.Errorf("the P0 bound %d from %s is below zero", bound.Bound, stamp(bound.At))
		}
	}
	return nil
}

// decodeStrict decodes line into into, refusing a member into has no field for.
func decodeStrict(line []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// Write writes w into dir, which it creates where it is missing: each input, then window.json,
// whose Files it fills with the sha256 of every input it wrote and whose Format it sets. Each
// line is written compact on a line of its own. An input of the signals is written where w holds
// it, as a file with no line where w holds it empty. dir is meant to be empty: an input file
// already there that w does not hold is left in place, and ReadWindow then refuses the window for
// it. A record of the signals ReadWindow would refuse (checkSignals) is refused before anything is
// written.
func (w Window) Write(dir string) error {
	w.Format = WindowFormat
	w.Files = make(map[string]string)
	if err := w.checkSignals(map[string]bool{launcherFile: w.Launcher != nil, providerFile: w.Provider != nil, queueDir: w.Queue != nil}); err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	write := func(name string, lines []json.RawMessage) error {
		data, err := encodeLines(name, lines)
		if err != nil {
			return err
		}
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, data, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		w.Files[name] = hex.EncodeToString(sum[:])
		return nil
	}
	if err := write(logFile, w.Log); err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	for _, key := range slices.Sorted(maps.Keys(w.Events)) {
		if !claim.IsIssueKey(key) {
			return fmt.Errorf("write the window to %s: events of %q, which is no issue key", dir, key)
		}
		if err := write(path.Join(eventsDir, key+".jsonl"), w.Events[key]); err != nil {
			return fmt.Errorf("write the window to %s: %w", dir, err)
		}
	}
	for _, repo := range slices.Sorted(maps.Keys(w.Merges)) {
		owner, name, ok := splitRepo(repo)
		if !ok {
			return fmt.Errorf("write the window to %s: merges of %q, which is no owner/repo", dir, repo)
		}
		lines, err := marshalLines(w.Merges[repo])
		if err != nil {
			return fmt.Errorf("write the window to %s: %w", dir, err)
		}
		if err := write(path.Join(mergesDir, owner+"__"+name+".jsonl"), lines); err != nil {
			return fmt.Errorf("write the window to %s: %w", dir, err)
		}
	}
	for _, input := range []struct {
		name  string
		held  bool
		lines func() ([]json.RawMessage, error)
	}{
		{providerFile, w.Provider != nil, func() ([]json.RawMessage, error) { return marshalLines(w.Provider) }},
		{launcherFile, w.Launcher != nil, func() ([]json.RawMessage, error) { return marshalLines(w.Launcher) }},
		{memoryFile, w.Memory != nil, func() ([]json.RawMessage, error) { return marshalLines(w.Memory) }},
		{settingsFile, w.Settings != nil, func() ([]json.RawMessage, error) { return w.Settings, nil }},
	} {
		if !input.held {
			continue
		}
		lines, err := input.lines()
		if err == nil {
			err = write(input.name, lines)
		}
		if err != nil {
			return fmt.Errorf("write the window to %s: %w", dir, err)
		}
	}
	for _, repo := range slices.Sorted(maps.Keys(w.Queue)) {
		owner, name, ok := splitRepo(repo)
		if !ok {
			return fmt.Errorf("write the window to %s: a merge queue of %q, which is no owner/repo", dir, repo)
		}
		if err := write(path.Join(queueDir, owner+"__"+name+".jsonl"), w.Queue[repo]); err != nil {
			return fmt.Errorf("write the window to %s: %w", dir, err)
		}
	}
	window, err := marshalWindow(w)
	if err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, windowFile), window, 0o644); err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	return nil
}

// encodeLines is lines as Write writes the input name: each line compact, on a line of its own.
func encodeLines(name string, lines []json.RawMessage) ([]byte, error) {
	var data bytes.Buffer
	for i, line := range lines {
		if err := json.Compact(&data, line); err != nil {
			return nil, fmt.Errorf("write %s line %d: %w", name, i+1, err)
		}
		data.WriteByte('\n')
	}
	return data.Bytes(), nil
}

// marshalWindow is w's window.json as Write writes it: its members with a JSON name, indented by
// two spaces, and a newline after.
func marshalWindow(w Window) ([]byte, error) {
	window, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(window, '\n'), nil
}

// replaceInput writes lines into the window in dir as its input name, in place of the file of that
// name the window holds or beside its other inputs where it holds none, then its window.json: head,
// the window as readWindowHead read it, with the sha256 of the file written in its files. Each is
// written as Write writes it, to a temporary file in its own directory that is renamed into place;
// no other file of dir is opened, so every other input keeps its bytes. The new head's record of the
// signals is checked (checkSignals), and both files are encoded, before either is written, so a
// refusal writes nothing.
func replaceInput(dir string, head Window, name string, lines []json.RawMessage) error {
	data, err := encodeLines(name, lines)
	if err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	sum := sha256.Sum256(data)
	files := make(map[string]string, len(head.Files)+1)
	maps.Copy(files, head.Files)
	files[name] = hex.EncodeToString(sum[:])
	head.Files = files
	if err := head.checkSignals(heldInputs(head.Files)); err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	window, err := marshalWindow(head)
	if err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{name, data}, {windowFile, window}} {
		if err := replaceFile(filepath.Join(dir, filepath.FromSlash(file.name)), file.data); err != nil {
			return fmt.Errorf("write the window to %s: %w", dir, err)
		}
	}
	return nil
}

// replaceFile writes data as file, whose directory it creates where it is missing, through a
// temporary file in that directory that it renames into place, so a reader of file reads either
// its bytes before or data whole. A write that fails leaves no temporary file.
func replaceFile(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*")
	if err != nil {
		return err
	}
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Chmod(0o644)
	}
	if closed := temp.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(temp.Name(), file)
	}
	if err != nil {
		os.Remove(temp.Name())
		return err
	}
	return nil
}

// marshalLines is each of values as a JSON line.
func marshalLines[T any](values []T) ([]json.RawMessage, error) {
	lines := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		line, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// splitRepo is repo, a repository as owner/name, split into its owner and name, and whether it is
// one: an owner and a name, neither empty, the name with no slash and the owner with no "__", which
// a merges file's name (<owner>__<name>.jsonl) joins them with.
func splitRepo(repo string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(repo, "/")
	return owner, name, ok && owner != "" && name != "" && !strings.Contains(name, "/") && !strings.Contains(owner, "__")
}

// inputOf is which input of the format name is — logFile, eventsDir, mergesDir, providerFile,
// launcherFile, memoryFile, queueDir or settingsFile — and the key it holds: the issue key of an
// events file, owner/repo of a merges or a queue file, "" for the others. A name that is not clean,
// relative and slash-separated, or that names no input, is refused.
func inputOf(name string) (kind, key string, err error) {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, "\\") {
		return "", "", fmt.Errorf("window.json names the file %q, which is no clean relative path", name)
	}
	switch name {
	case logFile, providerFile, launcherFile, memoryFile, settingsFile:
		return name, "", nil
	}
	dir, file := path.Split(name)
	base, jsonl := strings.CutSuffix(file, ".jsonl")
	switch {
	case !jsonl:
	case dir == eventsDir+"/" && claim.IsIssueKey(base):
		return eventsDir, base, nil
	case dir == mergesDir+"/" || dir == queueDir+"/":
		if owner, repo, ok := strings.Cut(base, "__"); ok && owner != "" && repo != "" {
			return strings.TrimSuffix(dir, "/"), owner + "/" + repo, nil
		}
	}
	return "", "", fmt.Errorf("window.json names the file %q, which is no input of the window's format", name)
}

// inputDirs is every directory of a window's format that holds input files.
var inputDirs = []string{eventsDir, mergesDir, path.Dir(providerFile), path.Dir(launcherFile), queueDir, path.Dir(settingsFile)}

// inputFiles is every input file dir holds, by its name relative to dir: log.jsonl where it is
// there, and every entry of the format's directories (inputDirs). A directory of the format that
// is missing holds none.
func inputFiles(dir string) ([]string, error) {
	var names []string
	if _, err := os.Stat(filepath.Join(dir, logFile)); err == nil {
		names = append(names, logFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, sub := range inputDirs {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			names = append(names, path.Join(sub, entry.Name()))
		}
	}
	return names, nil
}

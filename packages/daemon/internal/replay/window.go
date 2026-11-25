package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// The signals of R11, each by the name a window's signals map gives its record under.
const (
	SignalIdle      = "idle"
	SignalQueue     = "queue"
	SignalProvider  = "provider"
	SignalKills     = "kills"
	SignalApprovals = "approvals"
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
//   - merges/<owner>__<repo>.jsonl, the first-parent commits of a repository's base branches;
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
// the rate of the base's movement from. PR is the pull request the commit merged, where the first
// line of its message ends with GitHub's squash suffix ` (#<n>)`, and 0 for a commit with no such
// suffix; the draw reads it to leave a stretch's own merge out of the stretch's count (Stretch).
type Merge struct {
	SHA    string    `json:"sha"`
	At     time.Time `json:"at"`
	Branch string    `json:"branch"`
	PR     int       `json:"pr,omitempty"`
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
// names, each checked against its sha256 before it is read. It refuses a window of another format,
// an input that is missing or whose hash differs, a name in Files that is no input of the format, a
// line of an input that is not one JSON object or not of that input's form, an input file in dir
// that Files does not name, since nothing would check it, and a record of the signals no reader
// could trust (checkSignals). Other files of dir — a fixture's README.md and expect.json — are no
// part of the window and are not read.
func ReadWindow(dir string) (Window, error) {
	w, err := readWindowHead(dir)
	if err != nil {
		return Window{}, err
	}
	w.Events = make(map[string][]json.RawMessage)
	w.Merges = make(map[string][]Merge)
	for _, name := range slices.Sorted(maps.Keys(w.Files)) {
		kind, key, err := inputOf(name)
		if err != nil {
			return Window{}, fmt.Errorf("read the window in %s: %w", dir, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return Window{}, fmt.Errorf("read the window in %s: %w", dir, err)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != w.Files[name] {
			return Window{}, fmt.Errorf("read the window in %s: the sha256 of %s is %s, and window.json records %s",
				dir, name, hex.EncodeToString(sum[:]), w.Files[name])
		}
		lines, err := jsonLines(data)
		if err != nil {
			return Window{}, fmt.Errorf("read the window in %s: %s: %w", dir, name, err)
		}
		// refuse names the line of the input that does not read.
		refuse := func(i int, err error) (Window, error) {
			return Window{}, fmt.Errorf("read the window in %s: %s line %d: %w", dir, name, i+1, err)
		}
		switch kind {
		case logFile:
			w.Log = lines
		case eventsDir:
			w.Events[key] = lines
		case mergesDir:
			merges := make([]Merge, 0, len(lines))
			for i, line := range lines {
				var merge Merge
				if err := json.Unmarshal(line, &merge); err != nil {
					return refuse(i, err)
				}
				merges = append(merges, merge)
			}
			w.Merges[key] = merges
		case providerFile:
			w.Provider = make([]ProviderMinute, 0, len(lines))
			for i, line := range lines {
				minute, err := ParseProviderMinute(line)
				if err != nil {
					return refuse(i, err)
				}
				w.Provider = append(w.Provider, minute)
			}
		case launcherFile:
			w.Launcher = make([]LauncherLine, 0, len(lines))
			for i, line := range lines {
				var launched LauncherLine
				if err := decodeStrict(line, &launched); err != nil {
					return refuse(i, err)
				}
				if err := launched.check(); err != nil {
					return refuse(i, err)
				}
				w.Launcher = append(w.Launcher, launched)
			}
		case memoryFile:
			w.Memory = make([]MemorySample, 0, len(lines))
			for i, line := range lines {
				var sample MemorySample
				if err := decodeStrict(line, &sample); err != nil {
					return refuse(i, err)
				}
				if err := sample.check(); err != nil {
					return refuse(i, err)
				}
				w.Memory = append(w.Memory, sample)
			}
		case queueDir:
			for i, line := range lines {
				if _, err := ParseQueueLine(line); err != nil {
					return refuse(i, err)
				}
			}
			if w.Queue == nil {
				w.Queue = make(map[string][]json.RawMessage)
			}
			w.Queue[key] = append([]json.RawMessage{}, lines...)
		case settingsFile:
			for i, line := range lines {
				if _, err := ParseSettingsEvent(line); err != nil {
					return refuse(i, err)
				}
			}
			w.Settings = append([]json.RawMessage{}, lines...)
		}
	}
	present, err := inputFiles(dir)
	if err != nil {
		return Window{}, fmt.Errorf("read the window in %s: %w", dir, err)
	}
	for _, name := range present {
		if _, listed := w.Files[name]; !listed {
			return Window{}, fmt.Errorf("read the window in %s: %s is an input file window.json does not name, so nothing checks it", dir, name)
		}
	}
	if err := w.checkSignals(); err != nil {
		return Window{}, fmt.Errorf("read the window in %s: %w", dir, err)
	}
	return w, nil
}

// checkSignals refuses a record of the signals no reader could trust: a signal the signals map
// names that is none of R11's, a span that is empty, spans of one signal out of order or
// overlapping, a record of the kills with no launcher log, of the provider with no provider
// record or of the queue with no queue file, and a P0 bound below zero. Its refusal names the
// files by the names window.json gives them.
func (w Window) checkSignals() error {
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
	needs := map[string]bool{SignalKills: w.Launcher != nil, SignalProvider: w.Provider != nil, SignalQueue: w.Queue != nil}
	inputs := map[string]string{SignalKills: launcherFile, SignalProvider: providerFile, SignalQueue: "queue file"}
	for _, signal := range []string{SignalKills, SignalProvider, SignalQueue} {
		if len(w.Signals[signal]) > 0 && !needs[signal] {
			return fmt.Errorf("window.json names a record of %s, and the window holds no %s", signal, inputs[signal])
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
	if err := w.checkSignals(); err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	write := func(name string, lines []json.RawMessage) error {
		var data bytes.Buffer
		for i, line := range lines {
			if err := json.Compact(&data, line); err != nil {
				return fmt.Errorf("write %s line %d: %w", name, i+1, err)
			}
			data.WriteByte('\n')
		}
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, data.Bytes(), 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(data.Bytes())
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
	window, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, windowFile), append(window, '\n'), 0o644); err != nil {
		return fmt.Errorf("write the window to %s: %w", dir, err)
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

// jsonLines is data's lines, each one JSON object: a last line ended by a newline leaves no empty
// line after it, and any other empty line, or a line that is not an object, is refused. Each line
// is a part of data, which the caller writes nothing to after.
func jsonLines(data []byte) ([]json.RawMessage, error) {
	text := bytes.TrimSuffix(data, []byte("\n"))
	if len(text) == 0 {
		return nil, nil
	}
	split := bytes.Split(text, []byte("\n"))
	lines := make([]json.RawMessage, 0, len(split))
	for i, line := range split {
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("{")) || !json.Valid(trimmed) {
			return nil, fmt.Errorf("line %d is not one JSON object", i+1)
		}
		lines = append(lines, json.RawMessage(trimmed))
	}
	return lines, nil
}

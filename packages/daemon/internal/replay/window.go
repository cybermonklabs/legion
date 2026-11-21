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
// itself; the other three hold the inputs it names in its files map.
const (
	windowFile = "window.json"
	logFile    = "log.jsonl"
	eventsDir  = "events"
	mergesDir  = "merges"
)

// Window is one recorded window of a daemon's project: what the daemon was told and what it said
// over [From, To), read from a directory (ReadWindow) and written to one (Write). window.json
// holds every field with a JSON name; the inputs it names in Files are the three kinds of file
// beside it, held here as read:
//
//   - log.jsonl, the daemon's structured lines, one JSON object per line, exactly as its JSON
//     handler writes them on stderr (time, level, msg and the attributes), in the order written;
//   - events/<KEY>.jsonl, each issue's Dispatch events, one raw event per line, oldest first,
//     exactly as Dispatch's issue event log serves them (the shape intake.DecodeDispatchEvent
//     reads);
//   - merges/<owner>__<repo>.jsonl, the first-parent commits of a repository's base branches.
//
// A window is the record a replay runs from (RunConfig.Window) and the record a scorecard is read
// from (Score), so both read the same bytes, and the hash of each input in Files is what says
// they are the bytes collect wrote.
type Window struct {
	// Format is WindowFormat.
	Format int `json:"format"`
	// Project is the Dispatch project key of the daemon the window was recorded from.
	Project string `json:"project"`
	// From and To bound the window: the replay starts its daemon at From and stops at To.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Before is how far before From the inputs reach, so the state at From can be rebuilt from
	// them.
	Before Duration `json:"before"`
	// Caps is the admission cap the daemon ran with, each from its At on.
	Caps []CapAt `json:"caps"`
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
	// Experiments is the project's declared experiments (config.Project.ExperimentsDeclared).
	Experiments []config.Experiment `json:"experiments"`
	// Lines is every msg of a log line the collector kept, so a reader of the log can tell a kind
	// the daemon did not write in the window from a kind the window does not hold.
	Lines []string `json:"lines"`
	// Collector is the commit of cybermonklabs/legion whose collect wrote the window.
	Collector string `json:"collector"`
	// Files is the sha256, as lower-case hex, of every input file, by its path relative to the
	// window's directory, written with forward slashes.
	Files map[string]string `json:"files"`

	// Log is log.jsonl's lines, in the file's order.
	Log []json.RawMessage `json:"-"`
	// Events is each events/<KEY>.jsonl's lines, oldest first, by issue key.
	Events map[string][]json.RawMessage `json:"-"`
	// Merges is each merges file's commits, by repository as owner/repo.
	Merges map[string][]Merge `json:"-"`
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
// the rate of the base's movement from.
type Merge struct {
	SHA    string    `json:"sha"`
	At     time.Time `json:"at"`
	Branch string    `json:"branch"`
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

// ReadWindow reads the window in dir: window.json, then every input its Files names, each checked
// against its sha256 before it is read. It refuses a window of another format, an input that is
// missing or whose hash differs, a name in Files that is no input of the format, a line of an
// input that is not one JSON object, and an input file in dir that Files does not name, since
// nothing would check it. Other files of dir — a fixture's README.md and expect.json — are no
// part of the window and are not read.
func ReadWindow(dir string) (Window, error) {
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
					return Window{}, fmt.Errorf("read the window in %s: %s line %d: %w", dir, name, i+1, err)
				}
				merges = append(merges, merge)
			}
			w.Merges[key] = merges
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
	return w, nil
}

// Write writes w into dir, which it creates where it is missing: each input, then window.json,
// whose Files it fills with the sha256 of every input it wrote and whose Format it sets. Each
// line is written compact on a line of its own. dir is meant to be empty: an input file already
// there that w does not hold is left in place, and ReadWindow then refuses the window for it.
func (w Window) Write(dir string) error {
	w.Format = WindowFormat
	w.Files = make(map[string]string)
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
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.Contains(owner, "__") {
			return fmt.Errorf("write the window to %s: merges of %q, which is no owner/repo", dir, repo)
		}
		lines := make([]json.RawMessage, 0, len(w.Merges[repo]))
		for _, merge := range w.Merges[repo] {
			line, err := json.Marshal(merge)
			if err != nil {
				return fmt.Errorf("write the window to %s: %w", dir, err)
			}
			lines = append(lines, line)
		}
		if err := write(path.Join(mergesDir, owner+"__"+name+".jsonl"), lines); err != nil {
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

// inputOf is which input of the format name is — logFile, eventsDir or mergesDir — and the key
// it holds: the issue key of an events file, owner/repo of a merges file, "" for the log. A name
// that is not clean, relative and slash-separated, or that names no input, is refused.
func inputOf(name string) (kind, key string, err error) {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.Contains(name, "\\") {
		return "", "", fmt.Errorf("window.json names the file %q, which is no clean relative path", name)
	}
	if name == logFile {
		return logFile, "", nil
	}
	dir, file := path.Split(name)
	base, jsonl := strings.CutSuffix(file, ".jsonl")
	switch {
	case !jsonl:
	case dir == eventsDir+"/" && claim.IsIssueKey(base):
		return eventsDir, base, nil
	case dir == mergesDir+"/":
		if owner, repo, ok := strings.Cut(base, "__"); ok && owner != "" && repo != "" {
			return mergesDir, owner + "/" + repo, nil
		}
	}
	return "", "", fmt.Errorf("window.json names the file %q, which is no input of the window's format", name)
}

// inputFiles is every input file dir holds, by its name relative to dir: log.jsonl where it is
// there, and every entry of events/ and merges/. A directory of the format that is missing holds
// none.
func inputFiles(dir string) ([]string, error) {
	var names []string
	if _, err := os.Stat(filepath.Join(dir, logFile)); err == nil {
		names = append(names, logFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, sub := range []string{eventsDir, mergesDir} {
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
// line after it, and any other empty line, or a line that is not an object, is refused.
func jsonLines(data []byte) ([]json.RawMessage, error) {
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	split := strings.Split(text, "\n")
	lines := make([]json.RawMessage, 0, len(split))
	for i, line := range split {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
			return nil, fmt.Errorf("line %d is not one JSON object", i+1)
		}
		lines = append(lines, json.RawMessage(trimmed))
	}
	return lines, nil
}

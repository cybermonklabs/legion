package replay

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// DefaultConcurrency is how many issues a collect reads from Dispatch at once where its caller
// names no other bound: enough to read a project's few thousand event logs in minutes, few enough
// that Dispatch serves its other callers meanwhile.
const DefaultConcurrency = 8

// EventLog is the part of Dispatch a collect reads: an issue's event log after a sequence, every
// page of it, oldest first, each event as Dispatch serves it. dispatch.Client's EventsAfter is it,
// and sends GET alone.
type EventLog interface {
	EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error)
}

// Settings is the deployment's settings a window records (Window's fields of the same names),
// the ones that change what the engine and the admission pass decide, so a replay decides with the
// values the record was made under.
type Settings struct {
	TickInterval      time.Duration
	GateSteps         []record.DesignStep
	Linger            time.Duration
	ReviewRoundCap    int
	StalledTreeNotice time.Duration
	WorkerSilence     time.Duration
	WorkerBackground  time.Duration
}

// DaemonSettings is the settings a daemon runs with where its legion.yaml names none: each read
// from the default the configuration's key table writes for its key (config.ConfigReference). The
// tick interval's row derives that text from the value the daemon resolves; every other row's text
// is written by hand beside its value, so the collect test holds each one to the daemon's default
// (TestCollectDefaultsToTheDaemonsSettings). The design gate is the single step spec
// (config.DesignGateRootIssues).
func DaemonSettings() (Settings, error) {
	defaults := make(map[string]string)
	for _, key := range config.ConfigReference() {
		defaults[key.Name] = key.Default
	}
	number := func(name string) (int, error) {
		_, rest, _ := strings.Cut(defaults[name], "`")
		value, _, closed := strings.Cut(rest, "`")
		n, err := strconv.Atoi(value)
		if !closed || err != nil {
			return 0, fmt.Errorf("replay: the configuration's default of %s is %q, which names no number", name, defaults[name])
		}
		return n, nil
	}
	var s Settings
	for _, field := range []struct {
		name string
		unit time.Duration
		into *time.Duration
	}{
		{"controller_wake_interval_seconds", time.Second, &s.TickInterval},
		{"linger_hours", time.Hour, &s.Linger},
		{"stalled_tree_notice_hours", time.Hour, &s.StalledTreeNotice},
		{"worker_silence_minutes", time.Minute, &s.WorkerSilence},
		{"worker_background_hours", time.Hour, &s.WorkerBackground},
	} {
		n, err := number(field.name)
		if err != nil {
			return Settings{}, err
		}
		*field.into = time.Duration(n) * field.unit
	}
	rounds, err := number("review_round_cap")
	if err != nil {
		return Settings{}, err
	}
	s.ReviewRoundCap = rounds
	s.GateSteps = config.DesignGateRootIssues.Steps()
	return s, nil
}

// CollectConfig is one collection of a recorded window (Collect): the project and the span, the
// log files a person gathered, the Dispatch the issues' events are read from, what window.json
// records beside them, and the directory the window is written to.
type CollectConfig struct {
	// Project is the Dispatch project key of the daemon the lines are from.
	Project string
	// From and To bound the window, and Before is how far before From its inputs reach and After
	// how far past To (the run-out): the lines kept are those written in [From − Before,
	// To + After).
	From, To      time.Time
	Before, After time.Duration
	// Logs is each file or directory of the daemon's lines a person collected (every file under a
	// directory is read): the daemon's own standard error, `kubectl logs` output with or without
	// its timestamps, or a Cloud Logging export, rows of {timestamp, jsonPayload} as JSON lines or
	// as one JSON array; any of them gzipped or not.
	Logs []string
	// Dispatch is where each issue's events are read from.
	Dispatch EventLog
	// Concurrency bounds how many issues are read from Dispatch at once; 0 is DefaultConcurrency.
	Concurrency int
	// Settings is the deployment's settings window.json records (DaemonSettings where the
	// deployment set none).
	Settings Settings
	// Parts says which release, commit and slot rule ran each part of the span; none where the
	// caller does not know.
	Parts []Part
	// Collector is the commit of cybermonklabs/legion the collect runs from.
	Collector string
	// Out is the directory the window is written to, which must be missing or empty.
	Out string
	// Wait is how a read Dispatch refused for now waits before it is tried again; nil waits on a
	// timer. A test waits no time.
	Wait func(ctx context.Context, d time.Duration) error
}

// Collected is the window a collect wrote, and what it read and left out on the way.
type Collected struct {
	Window Window
	// Read is how many daemon lines the log files held, and Kept how many the window holds: the
	// lines of a kind the trace reads (TraceReads), written in [From − Before, To + After), each once.
	Read, Kept int
	// Duplicates is how many lines were left out as a line already kept, which two log files that
	// cover the same time give.
	Duplicates int
	// NotLines is how many lines of the files were no daemon line: a line of other output in
	// `kubectl logs`, a line cut short, a row of an export that holds no jsonPayload.
	NotLines int
	// Missing is each issue key a line or an event names that Dispatch answers it does not have,
	// sorted; the window holds no events of it.
	Missing []string
}

// Collect reads a recorded window and writes it to cfg.Out. From the log files it keeps the lines
// of every kind the trace reads, written in [From − Before, To + After), in time order (a line read
// in two files once); from Dispatch it reads the events of every issue those lines name, and of
// every blocker, parent and child those issues' events name, in any project, each issue's from its
// first event and up to To + After, at most cfg.Concurrency at once, each read Dispatch refuses for
// now (429, a 5xx, no answer) tried again with a growing wait. It sends Dispatch GET alone.
// window.json records the project, the span, Before, the run-out After, the cap of each change the
// slot lines and the daemon's start lines show, the line kinds the log holds, cfg's settings, parts
// and collector, and the sha256 of each file. A line of a kind the trace reads that its reader
// refuses fails the collect, naming the file and the line, so a window is never written that the
// trace cannot read.
func Collect(ctx context.Context, cfg CollectConfig) (Collected, error) {
	if err := cfg.check(); err != nil {
		return Collected{}, err
	}
	end := cfg.To.Add(cfg.After)
	lines, read, err := readLogs(cfg.Logs, cfg.From.Add(-cfg.Before), end)
	if err != nil {
		return Collected{}, err
	}
	collected := Collected{Read: read.lines, Kept: len(lines), Duplicates: read.duplicates, NotLines: read.notLines}

	named := make(map[string]bool)
	kinds := make(map[string]bool)
	w := Window{Project: cfg.Project, From: cfg.From.UTC(), To: cfg.To.UTC(), Before: Duration(cfg.Before), After: Duration(cfg.After),
		Caps: []CapAt{}, Parts: append([]Part{}, cfg.Parts...), TickInterval: Duration(cfg.Settings.TickInterval),
		GateSteps: append([]record.DesignStep{}, cfg.Settings.GateSteps...), Linger: Duration(cfg.Settings.Linger),
		ReviewRoundCap: cfg.Settings.ReviewRoundCap, StalledTreeNotice: Duration(cfg.Settings.StalledTreeNotice),
		WorkerSilence: Duration(cfg.Settings.WorkerSilence), WorkerBackground: Duration(cfg.Settings.WorkerBackground),
		Experiments: []config.Experiment{}, Collector: cfg.Collector, Log: make([]json.RawMessage, 0, len(lines))}
	for _, line := range lines {
		w.Log = append(w.Log, line.raw)
		kinds[line.parsed.Head().Msg] = true
		for _, key := range keysOfLine(line.parsed) {
			if claim.IsIssueKey(key) {
				named[key] = true
			}
		}
		if limit, ok := capOfLine(line.parsed); ok && (len(w.Caps) == 0 || w.Caps[len(w.Caps)-1].Cap != limit) {
			w.Caps = append(w.Caps, CapAt{At: line.parsed.Head().Time.UTC(), Cap: limit})
		}
	}
	w.Lines = slices.Sorted(maps.Keys(kinds))
	if w.Lines == nil {
		w.Lines = []string{}
	}

	reader := eventReader{log: cfg.Dispatch, to: end, wait: cfg.Wait, limit: cfg.Concurrency, events: map[string][]json.RawMessage{}}
	if reader.wait == nil {
		reader.wait = backoff.Sleep
	}
	if reader.limit <= 0 {
		reader.limit = DefaultConcurrency
	}
	first := slices.Sorted(maps.Keys(named))
	if err := reader.read(ctx, first); err != nil {
		return Collected{}, err
	}
	related := make(map[string]bool)
	for _, key := range first {
		for _, event := range reader.events[key] {
			names, err := issuesAnEventNames(event)
			if err != nil {
				return Collected{}, fmt.Errorf("replay: collect: an event of %s: %w", key, err)
			}
			for _, name := range names {
				if claim.IsIssueKey(name) && !named[name] {
					related[name] = true
				}
			}
		}
	}
	if err := reader.read(ctx, slices.Sorted(maps.Keys(related))); err != nil {
		return Collected{}, err
	}
	w.Events = reader.events
	w.Merges = map[string][]Merge{}
	collected.Missing = slices.Sorted(slices.Values(reader.missing))
	if err := w.Write(cfg.Out); err != nil {
		return Collected{}, fmt.Errorf("replay: collect: %w", err)
	}
	if collected.Window, err = ReadWindow(cfg.Out); err != nil {
		return Collected{}, fmt.Errorf("replay: collect: the window written does not read back: %w", err)
	}
	return collected, nil
}

// check refuses a CollectConfig no collect can run: a project that is no Dispatch project key, an
// empty span, a negative reach or run-out, no log, no Dispatch, no collector, settings no daemon
// runs with, a part that names no rule, or an Out that holds files already.
func (cfg CollectConfig) check() error {
	switch {
	case !claim.IsIssueKey(cfg.Project + "-1"):
		return fmt.Errorf("replay: collect: %q is no Dispatch project key", cfg.Project)
	case !cfg.From.Before(cfg.To):
		return fmt.Errorf("replay: collect: the span [%s, %s) is empty", cfg.From.Format(time.RFC3339), cfg.To.Format(time.RFC3339))
	case cfg.Before < 0:
		return fmt.Errorf("replay: collect: the reach before the span is %s, below zero", cfg.Before)
	case cfg.After < 0:
		return fmt.Errorf("replay: collect: the run-out after the span is %s, below zero", cfg.After)
	case len(cfg.Logs) == 0:
		return errors.New("replay: collect: no log file is named")
	case cfg.Dispatch == nil:
		return errors.New("replay: collect: no Dispatch is named")
	case cfg.Collector == "":
		return errors.New("replay: collect: the collector's commit is not known")
	case cfg.Settings.TickInterval <= 0 || cfg.Settings.ReviewRoundCap <= 0:
		return errors.New("replay: collect: the tick interval and the review round cap must be above zero")
	}
	for _, step := range cfg.Settings.GateSteps {
		if !step.Valid() {
			return fmt.Errorf("replay: collect: %q is no design step (%s)", step, record.StepWords())
		}
	}
	for _, part := range cfg.Parts {
		if !part.From.Before(part.To) || part.Rule == "" {
			return fmt.Errorf("replay: collect: the part %+v has an empty span or no rule", part)
		}
	}
	return emptyDir(cfg.Out)
}

// emptyDir refuses a dir that holds anything: a window is written into a directory of its own.
// A missing dir is empty, and is made where the window is written.
func emptyDir(dir string) error {
	if dir == "" {
		return errors.New("replay: no output directory is named")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("replay: read the output directory: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("replay: the output directory %s holds %s already; name a missing or empty one", dir, entries[0].Name())
	}
	return nil
}

// keptLine is one line a collect keeps: the bytes the daemon wrote and the trace's reading of
// them.
type keptLine struct {
	raw    json.RawMessage
	parsed LogLine
}

// logCounts is what reading the log files counted beside the lines kept (Collected).
type logCounts struct {
	lines, duplicates, notLines int
}

// readLogs reads every daemon line of the files and directories paths name, in the order paths
// gives and, under a directory, by file name, and keeps the lines of a kind the trace reads
// written in [from, to), each once, ordered by the time the line carries (two lines of one time
// in the order read).
func readLogs(paths []string, from, to time.Time) ([]keptLine, logCounts, error) {
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, logCounts{}, fmt.Errorf("replay: collect: %w", err)
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		err = filepath.WalkDir(path, func(file string, entry fs.DirEntry, err error) error {
			if err == nil && entry.Type().IsRegular() {
				files = append(files, file)
			}
			return err
		})
		if err != nil {
			return nil, logCounts{}, fmt.Errorf("replay: collect: %w", err)
		}
	}
	var (
		kept   []keptLine
		counts logCounts
		seen   = make(map[[sha256.Size]byte]bool)
	)
	keep := func(file string, at int, element []byte) error {
		line, msg, ok := daemonLine(element)
		if !ok {
			counts.notLines++
			return nil
		}
		counts.lines++
		if !TraceReads(msg) {
			return nil
		}
		parsed, err := ParseLogLine(line)
		if err != nil {
			return fmt.Errorf("replay: collect: %s line %d: %w", file, at, err)
		}
		if when := parsed.Head().Time; when.Before(from) || !when.Before(to) {
			return nil
		}
		identity, err := lineIdentity(line)
		if err != nil {
			return fmt.Errorf("replay: collect: %s line %d: %w", file, at, err)
		}
		if seen[identity] {
			counts.duplicates++
			return nil
		}
		seen[identity] = true
		kept = append(kept, keptLine{raw: bytes.Clone(line), parsed: parsed})
		return nil
	}
	for _, file := range files {
		if err := readLogFile(file, keep); err != nil {
			return nil, logCounts{}, err
		}
	}
	slices.SortStableFunc(kept, func(a, b keptLine) int { return a.parsed.Head().Time.Compare(b.parsed.Head().Time) })
	return kept, counts, nil
}

// readLogFile hands keep each JSON element of file with its place in the file: the entries of a
// file that holds one JSON array, and otherwise each line, its `kubectl logs` prefix (the time
// --timestamps writes, the [pod/…] --prefix writes) cut off. A line that does not start a JSON
// object is handed on as it is, for keep to count as no daemon line. A gzipped file is read
// through its compression, whatever its name.
func readLogFile(file string, keep func(file string, at int, element []byte) error) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("replay: collect: %w", err)
	}
	defer f.Close()
	buffered := bufio.NewReaderSize(f, 1<<20)
	if magic, _ := buffered.Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		unzipped, err := gzip.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("replay: collect: %s: %w", file, err)
		}
		defer unzipped.Close()
		buffered = bufio.NewReaderSize(unzipped, 1<<20)
	}
	if first, err := firstByte(buffered); err != nil {
		return fmt.Errorf("replay: collect: %s: %w", file, err)
	} else if first == '[' {
		decoder := json.NewDecoder(buffered)
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("replay: collect: %s: %w", file, err)
		}
		for at := 1; decoder.More(); at++ {
			var element json.RawMessage
			if err := decoder.Decode(&element); err != nil {
				return fmt.Errorf("replay: collect: %s entry %d: %w", file, at, err)
			}
			if err := keep(file, at, element); err != nil {
				return err
			}
		}
		return nil
	}
	scanner := bufio.NewScanner(buffered)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for at := 1; scanner.Scan(); at++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// The prefix is optional, so it matches every line, the empty prefix included.
		line = line[kubectlPrefix.FindIndex(line)[1]:]
		if err := keep(file, at, line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("replay: collect: %s: %w", file, err)
	}
	return nil
}

// kubectlPrefix is what `kubectl logs` writes before a container's own line: the pod and
// container --prefix names, then the time --timestamps writes, either or both.
var kubectlPrefix = regexp.MustCompile(`^(?:\[[^\]]*\]\s+)?(?:\d{4}-\d{2}-\d{2}T[0-9:.]+(?:Z|[+-]\d{2}:\d{2})\s+)?`)

// firstByte is the first byte of r that is not white space, left unread; 0 for an empty r.
func firstByte(r *bufio.Reader) (byte, error) {
	for {
		b, err := r.Peek(1)
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if !strings.ContainsRune(" \t\r\n", rune(b[0])) {
			return b[0], nil
		}
		if _, err := r.ReadByte(); err != nil {
			return 0, err
		}
	}
}

// daemonLine is the daemon's own line in element and the msg it was written under: element itself
// where it is a line of the daemon's stderr, or the jsonPayload of a Cloud Logging row. ok is false
// for an element that is no JSON object with a string msg.
func daemonLine(element []byte) (line []byte, msg string, ok bool) {
	var probe struct {
		Payload json.RawMessage `json:"jsonPayload"`
		Msg     *string         `json:"msg"`
	}
	if !bytes.HasPrefix(element, []byte("{")) || json.Unmarshal(element, &probe) != nil {
		return nil, "", false
	}
	if len(probe.Payload) > 0 {
		return daemonLine(bytes.TrimSpace(probe.Payload))
	}
	if probe.Msg == nil {
		return nil, "", false
	}
	return element, *probe.Msg, true
}

// lineIdentity is what tells two copies of one line apart from two lines: the hash of the line's
// attributes in one order, so a line read from the daemon's stderr and the same line from a Cloud
// Logging export, whose attributes come sorted, are one line.
func lineIdentity(line []byte) ([sha256.Size]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return [sha256.Size]byte{}, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

// keysOfLine is every issue key a line names: its issue, its tree, the blockers it waits on, the
// issue its claim token names, the issues a pass's slots and candidates name, and the issues a
// merged issue's close waits for.
func keysOfLine(line LogLine) []string {
	switch l := line.(type) {
	case PhaseChanged:
		return []string{l.Issue, l.Tree}
	case SlotTaken:
		return []string{l.Issue}
	case SlotReleased:
		return []string{l.Issue}
	case WaitsOnIssues:
		return append([]string{l.Issue}, l.Blockers...)
	case SlotBackWorkReturned:
		return []string{l.Issue}
	case SlotBackWaitEnded:
		return []string{l.Issue}
	case WaitsForChildren:
		return []string{l.Issue, l.Tree}
	case WaitsOnPerson:
		return []string{l.Issue, l.Tree}
	case GateOpened:
		return []string{l.Issue}
	case GateClosed:
		return []string{l.Issue}
	case GateRegistered:
		return []string{l.Issue}
	case HoldReleased:
		return []string{l.Issue}
	case LeftWorkflow:
		return []string{l.Issue, l.Tree}
	case TreeLingers:
		return []string{l.Tree}
	case LingerClosed:
		return []string{l.Tree}
	case TurnStarted:
		return []string{l.Claim.Issue}
	case TurnEnded:
		return []string{l.Claim.Issue}
	case Launched:
		return []string{l.Claim.Issue}
	case Suspended:
		return []string{l.Claim.Issue}
	case ClaimFailed:
		return []string{l.Claim.Issue}
	case ProcessEnded:
		return []string{l.Claim.Issue}
	case StartFailure:
		return []string{l.Claim.Issue}
	case QueueLeftNothingRead:
		return []string{l.Issue}
	case QueueLeftReadOnce:
		return []string{l.Issue}
	case PassMovedSlots:
		keys := make([]string, 0, len(l.Given)+len(l.Taken)+len(l.Passed))
		for _, given := range l.Given {
			keys = append(keys, given.Issue)
		}
		for _, taken := range l.Taken {
			keys = append(keys, taken.Issue)
		}
		for _, passed := range l.Passed {
			keys = append(keys, passed.Issue)
		}
		return keys
	case PhaseHeld:
		return []string{l.Issue, l.Tree}
	case RootJoined:
		return []string{l.Issue, l.Tree}
	case CheckClosed:
		return []string{l.Issue}
	case MergeForwardAsked:
		return []string{l.Issue}
	case MergeForwardLanded:
		return []string{l.Issue}
	case MergedCloseWaits:
		return append([]string{l.Issue, l.Tree}, l.WaitingFor...)
	}
	return nil
}

// capOfLine is the admission cap a line shows the daemon ran with: a slot line's cap, or the cap a
// daemon process started under.
func capOfLine(line LogLine) (int, bool) {
	switch l := line.(type) {
	case SlotTaken:
		return l.Cap, true
	case SlotReleased:
		return l.Cap, true
	case DaemonStarted:
		return l.AdmissionCap, true
	}
	return 0, false
}

// issuesAnEventNames is every issue a Dispatch event names as related to its own: an issue
// event's blockers and parent, and the child a parent's child.added, child.removed or
// child.status names.
func issuesAnEventNames(event json.RawMessage) ([]string, error) {
	var named struct {
		Payload struct {
			BlockedBy []string `json:"blocked_by"`
			Parent    *string  `json:"parent"`
			ChildKey  string   `json:"child_key"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(event, &named); err != nil {
		return nil, err
	}
	names := append([]string{}, named.Payload.BlockedBy...)
	if named.Payload.Parent != nil {
		names = append(names, *named.Payload.Parent)
	}
	if named.Payload.ChildKey != "" {
		names = append(names, named.Payload.ChildKey)
	}
	return names, nil
}

// eventReader reads issues' event logs from Dispatch for a collect, limit at once, keeping each
// issue's events before to and naming each issue Dispatch does not have.
type eventReader struct {
	log   EventLog
	to    time.Time
	wait  func(ctx context.Context, d time.Duration) error
	limit int

	mu      sync.Mutex
	events  map[string][]json.RawMessage
	missing []string
}

// read reads the event log of each of keys.
func (r *eventReader) read(ctx context.Context, keys []string) error {
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(r.limit)
	for _, key := range keys {
		group.Go(func() error {
			var events []json.RawMessage
			err := retry(ctx, r.wait, retryableDispatch, func() (err error) {
				events, err = r.log.EventsAfter(ctx, key, 0)
				return err
			})
			switch {
			case dispatch.Missing(err):
				r.mu.Lock()
				defer r.mu.Unlock()
				r.missing = append(r.missing, key)
				return nil
			case err != nil:
				return fmt.Errorf("replay: collect: read the events of %s from Dispatch: %w", key, err)
			}
			var before []json.RawMessage
			for _, event := range events {
				var at struct {
					CreatedAt *time.Time `json:"created_at"`
				}
				if err := json.Unmarshal(event, &at); err != nil {
					return fmt.Errorf("replay: collect: an event of %s does not read: %w", key, err)
				}
				if at.CreatedAt == nil {
					return fmt.Errorf("replay: collect: an event of %s carries no created_at", key)
				}
				if at.CreatedAt.Before(r.to) {
					before = append(before, event)
				}
			}
			if len(before) > 0 {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.events[key] = before
			}
			return nil
		})
	}
	return group.Wait()
}

// retryAttempts is how many times a read that a server refuses for now is made before its refusal
// is the answer, and retryFirstWait and retryLongestWait bound the wait before each next attempt,
// which doubles from the first: about a minute in all.
const (
	retryAttempts    = 7
	retryFirstWait   = time.Second
	retryLongestWait = 30 * time.Second
)

// retry runs attempt until it succeeds, it fails with an error retryable does not call one a
// later attempt may clear, or retryAttempts attempts have failed, waiting with wait before each
// next attempt.
func retry(ctx context.Context, wait func(context.Context, time.Duration) error, retryable func(error) bool, attempt func() error) error {
	delay := retryFirstWait
	for n := 1; ; n++ {
		err := attempt()
		if err == nil || n == retryAttempts || !retryable(err) {
			return err
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
		delay = min(2*delay, retryLongestWait)
	}
}

// retryableDispatch says whether Dispatch refused a read for now: it answered 429 or a 5xx, or it
// gave no answer at all (dispatch.TransientError).
func retryableDispatch(err error) bool {
	var refusal *dispatch.Error
	if errors.As(err, &refusal) {
		return refusal.Status == http.StatusTooManyRequests || refusal.Status >= 500
	}
	var transient *dispatch.TransientError
	return errors.As(err, &transient)
}

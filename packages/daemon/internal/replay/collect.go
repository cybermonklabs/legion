package replay

import (
	"bufio"
	"bytes"
	"cmp"
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
	"net/url"
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

// EventLog is the part of Dispatch a collect reads each issue from: an issue's event log after a
// sequence, every page of it, oldest first, each event as Dispatch serves it. dispatch.Client's
// EventsAfter is it, and sends GET alone.
type EventLog interface {
	EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error)
}

// DispatchReader is every part of Dispatch a collect reads, with GET alone: each issue's event log
// (EventLog); a page of the whole event log, whatever owns each event, the events with an id above
// after, at most limit of them, oldest first, each as the issue event log serves it (GET
// /api/v1/events/log?after=&limit=); and a page of a project's issues of every status, the keys
// of the listing from offset, at most limit of them, with the number of issues the listing holds
// (GET /api/v1/issues?project=&limit=&offset=). NewDispatchReader is it.
type DispatchReader interface {
	EventLog
	LogAfter(ctx context.Context, after int64, limit int) ([]json.RawMessage, error)
	IssuesPage(ctx context.Context, project string, offset, limit int) (keys []string, total int, err error)
}

// The largest pages Dispatch serves of the whole event log and of a project's issues.
const (
	eventLogPage = 1000
	issuesPage   = 250
)

// NewDispatchReader is the DispatchReader of the Dispatch at base, read with token, which is sent
// as the Authorization header alone: the daemon's Dispatch client for each issue's events, which
// presents it through dispatch.BearerTransport, and a GET of its own for the event log and the
// listing, which that client does not read.
func NewDispatchReader(base, token string) DispatchReader {
	return dispatchReader{HTTPClient: dispatch.New(base, dispatch.BearerTransport(token, nil)), base: strings.TrimRight(base, "/"), token: token,
		client: &http.Client{Timeout: time.Minute}}
}

// dispatchReader is NewDispatchReader's DispatchReader.
type dispatchReader struct {
	*dispatch.HTTPClient
	base, token string
	client      *http.Client
}

// LogAfter reads a page of the whole event log (DispatchReader).
func (r dispatchReader) LogAfter(ctx context.Context, after int64, limit int) ([]json.RawMessage, error) {
	query := url.Values{}
	query.Set("after", strconv.FormatInt(after, 10))
	query.Set("limit", strconv.Itoa(limit))
	var page []json.RawMessage
	if err := r.get(ctx, "/api/v1/events/log?"+query.Encode(), &page); err != nil {
		return nil, err
	}
	return page, nil
}

// IssuesPage reads a page of a project's issues (DispatchReader).
func (r dispatchReader) IssuesPage(ctx context.Context, project string, offset, limit int) ([]string, int, error) {
	query := url.Values{}
	query.Set("project", project)
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	var page struct {
		Issues []struct {
			Key string `json:"key"`
		} `json:"issues"`
		Total *int `json:"total"`
	}
	if err := r.get(ctx, "/api/v1/issues?"+query.Encode(), &page); err != nil {
		return nil, 0, err
	}
	if page.Total == nil {
		return nil, 0, fmt.Errorf("Dispatch's page of %s's issues at offset %d names no total", project, offset)
	}
	keys := make([]string, 0, len(page.Issues))
	for _, issue := range page.Issues {
		keys = append(keys, issue.Key)
	}
	return keys, *page.Total, nil
}

// get reads target, a path of Dispatch's API with its query, into into, answering as the
// daemon's client answers: a status other than 2xx as a *dispatch.Error, and no answer as a
// *dispatch.TransientError unless the transport refused it (dispatch.TransportRefusal), so retry
// and retryableDispatch read both readers' refusals alike.
func (r dispatchReader) get(ctx context.Context, target string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.token)
	unanswered := func(err error) error {
		if dispatch.TransportRefusal(err) {
			return err
		}
		return &dispatch.TransientError{Err: fmt.Errorf("call Dispatch GET %s: %w", target, err)}
	}
	response, err := r.client.Do(request)
	if err != nil {
		return unanswered(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return unanswered(err)
	}
	if response.StatusCode/100 != 2 {
		var refusal struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &refusal) != nil || refusal.Error == "" {
			refusal.Error = cmp.Or(strings.TrimSpace(string(body[:min(len(body), 300)])), response.Status)
		}
		return &dispatch.Error{Status: response.StatusCode, Code: refusal.Code, Message: refusal.Error}
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode Dispatch GET %s: %w", target, err)
	}
	return nil
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
// log files a person gathered, the deployment's records of the signals, the Dispatch the issues'
// events and the project's settings are read from, what window.json records beside them, and the
// directory the window is written to.
type CollectConfig struct {
	// Project is the Dispatch project key of the daemon the lines are from.
	Project string
	// From and To bound the window, and Before is how far before From its inputs reach and After
	// how far past To (the run-out): the lines kept are those written in [From − Before,
	// To + After), the inputs' reach.
	From, To      time.Time
	Before, After time.Duration
	// Logs is each file or directory of the daemon's lines a person collected (every file under a
	// directory is read): the daemon's own standard error, `kubectl logs` output with or without
	// its timestamps, or a Cloud Logging export, rows of {timestamp, jsonPayload} as JSON lines or
	// as one JSON array; any of them gzipped or not.
	Logs []string
	// Provider is the file of the deployment's record of the model provider, one line a minute in
	// the form ParseProviderMinute reads (t25's provider-record script writes it), gzipped or not;
	// "" where the deployment gave none.
	Provider string
	// LauncherLogs is each file or directory of the launcher's lines of the project's issue pods:
	// rows of {timestamp, pod, text} (t25's export), Cloud Logging entries with timestamp,
	// resource.labels.pod_name and textPayload, or a window's own lines, as JSON lines or as one
	// JSON array, gzipped or not (ParseLauncherEntry). Under a directory, only the files named
	// *.jsonl, *.jsonl.gz, *.json and *.json.gz are read (signalInputFile), so an export's manifest
	// beside them is not; a file named here is read whatever its name.
	LauncherLogs []string
	// Memory is each file or directory of the pods' memory: rows of {minute, pod, memory_type,
	// bytes} (t25's export, a series for each memory type), or a window's own {minute, pod,
	// bytes}, as JSON lines or as one JSON array, gzipped or not, a directory read as
	// LauncherLogs's is.
	Memory []string
	// DefaultAssignee is the project's default assignee at the start of the inputs' reach, which
	// the deployment gives where no settings event before that start names it; "" where it gives
	// none.
	DefaultAssignee string
	// Dispatch is where each issue's events, the event log and the project's issues are read from.
	Dispatch DispatchReader
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
	// Missing is each issue key a line, an event or the project's listing names that Dispatch
	// answers it does not have, sorted; the window holds no events of it.
	Missing []string
	// SettingsEvents is how many settings events of the project the event log held up to the
	// reach's end, and ProjectIssues how many issues Dispatch's listing of the project holds.
	SettingsEvents, ProjectIssues int
}

// Collect reads a recorded window and writes it to cfg.Out. From the log files it keeps the lines
// of every kind the trace reads, written in [From − Before, To + After), the inputs' reach, in time
// order (a line read in two files once); from the deployment's records of the signals it keeps
// each one's lines in the reach: the provider's minutes, the launcher's lines as the window's rows
// in time order (a line read twice once), and the pods' memory as each pod's series summed a
// minute. From Dispatch, with GET alone, it pages the whole event log from its first event to the
// reach's end and keeps the project's settings events, the ones before the reach included, since
// they set what holds at its start; it lists every issue of the project; and it reads the events
// of every issue the lines name, of every blocker, parent and child those issues' events name, in
// any project, and of every issue of the project the listing or the event log names, each issue's
// from its first event up to the reach's end, at most cfg.Concurrency at once. Each read Dispatch
// refuses for now (429, a 5xx, no answer) is tried again with a growing wait.
// window.json records the project, the span, Before, the run-out After, the cap of each change the
// slot lines and the daemon's start lines show, the line kinds the log holds, cfg's settings, parts,
// collector and default assignee, the sha256 of each file, and the spans of each signal's record
// (collectedSignals). A line of a kind the trace reads that its reader refuses fails the collect,
// naming the file and the line, so a window is never written that the trace cannot read; so does a
// line of a signal's record its reader refuses.
func Collect(ctx context.Context, cfg CollectConfig) (Collected, error) {
	if err := cfg.check(); err != nil {
		return Collected{}, err
	}
	reach, end := cfg.From.Add(-cfg.Before), cfg.To.Add(cfg.After)
	lines, read, err := readLogs(cfg.Logs, reach, end)
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
		Experiments: []config.Experiment{}, Collector: cfg.Collector, DefaultAssignee: cfg.DefaultAssignee,
		Log: make([]json.RawMessage, 0, len(lines))}
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
	if cfg.Provider != "" {
		if w.Provider, err = readProvider(cfg.Provider, reach, end); err != nil {
			return Collected{}, err
		}
	}
	if len(cfg.LauncherLogs) > 0 {
		if w.Launcher, err = readLauncherLogs(cfg.LauncherLogs, reach, end); err != nil {
			return Collected{}, err
		}
	}
	if len(cfg.Memory) > 0 {
		if w.Memory, err = readMemory(cfg.Memory, reach, end); err != nil {
			return Collected{}, err
		}
	}

	wait := cfg.Wait
	if wait == nil {
		wait = backoff.Sleep
	}
	settings, logged, err := readProjectLog(ctx, cfg.Dispatch, cfg.Project, end, wait)
	if err != nil {
		return Collected{}, err
	}
	w.Settings = settings
	listed, err := listProjectIssues(ctx, cfg.Dispatch, cfg.Project, wait)
	if err != nil {
		return Collected{}, err
	}
	collected.SettingsEvents, collected.ProjectIssues = len(settings), len(listed)
	if w.Signals, err = collectedSignals(w, reach, end); err != nil {
		return Collected{}, err
	}

	reader := eventReader{log: cfg.Dispatch, to: end, wait: wait, limit: cfg.Concurrency, events: map[string][]json.RawMessage{}}
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
	// Every issue of the project, so a signal that reads every ask of the project reads them all:
	// the listing's, and the event log's too, since a listing read a page at a time can miss an
	// issue whose status or rank changed between two pages.
	project := make(map[string]bool)
	for _, key := range slices.Concat(listed, logged) {
		if claim.IsIssueKey(key) && !named[key] && !related[key] {
			project[key] = true
		}
	}
	if err := reader.read(ctx, slices.Sorted(maps.Keys(project))); err != nil {
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

// collectedSignals is the spans of each signal's record a window collected over the reach
// [reach, end) holds: the idle share's over the reach, from the daemon's lines; the kills' over
// the reach where the deployment gave the launcher's lines; the provider's over each run of
// consecutive minutes of its record with a limit in force, so a minute before the first
// observation of the limit, or one the record lacks, has no record (the architect's D13); and the
// approvals' from the moment the project's default assignee is known to the reach's end: from the
// reach's start where the deployment gave it or a settings event before that start set it, else
// from the first settings event in the reach that sets it, and nowhere where none does. The
// queue's record is collect-queue's to write.
func collectedSignals(w Window, reach, end time.Time) (map[string][]Span, error) {
	whole := []Span{{From: reach.UTC(), To: end.UTC()}}
	signals := map[string][]Span{SignalIdle: whole}
	if w.Launcher != nil {
		signals[SignalKills] = whole
	}
	var limited []Span
	for _, minute := range w.Provider {
		if minute.Limit == nil {
			continue
		}
		if last := len(limited) - 1; last >= 0 && limited[last].To.Equal(minute.Minute) {
			limited[last].To = minute.Minute.Add(time.Minute)
		} else {
			limited = append(limited, Span{From: minute.Minute, To: minute.Minute.Add(time.Minute)})
		}
	}
	if limited != nil {
		signals[SignalProvider] = limited
	}
	known := w.DefaultAssignee != ""
	var firstSet time.Time
	for _, raw := range w.Settings {
		event, err := ParseSettingsEvent(raw)
		if err != nil {
			return nil, fmt.Errorf("replay: collect: %w", err)
		}
		switch {
		case event.Type != defaultAssigneeEvent:
		case event.At.Before(reach):
			known = true
		case firstSet.IsZero() || event.At.Before(firstSet):
			firstSet = event.At
		}
	}
	switch {
	case known:
		signals[SignalApprovals] = whole
	case !firstSet.IsZero():
		signals[SignalApprovals] = []Span{{From: firstSet, To: end.UTC()}}
	}
	return signals, nil
}

// readProjectLog pages Dispatch's whole event log from its first event (DispatchReader's
// LogAfter, eventLogPage events a page, each page Dispatch refuses for now tried again with a
// growing wait), until a page holds an event created at or after end or the log ends, and keeps,
// of the events created before end, the project's settings events, verbatim and in the log's
// order, and the key of each issue of the project an event names. It refuses a page whose events
// do not read or whose ids do not rise, and a settings event ParseSettingsEvent refuses.
func readProjectLog(ctx context.Context, d DispatchReader, project string, end time.Time,
	wait func(context.Context, time.Duration) error) ([]json.RawMessage, []string, error) {
	settings := []json.RawMessage{}
	keys := make(map[string]bool)
	for after := int64(0); ; {
		var page []json.RawMessage
		err := retry(ctx, wait, retryableDispatch, func() (err error) {
			page, err = d.LogAfter(ctx, after, eventLogPage)
			return err
		})
		if err != nil {
			return nil, nil, fmt.Errorf("replay: collect: read Dispatch's event log after event %d: %w", after, err)
		}
		done := len(page) < eventLogPage
		for _, raw := range page {
			var head struct {
				ID        *int64     `json:"id"`
				IssueKey  *string    `json:"issue_key"`
				Project   *string    `json:"project"`
				Type      string     `json:"type"`
				CreatedAt *time.Time `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &head); err != nil || head.ID == nil || head.CreatedAt == nil {
				return nil, nil, fmt.Errorf("replay: collect: an event of Dispatch's event log after event %d does not read as one with its id and created_at", after)
			}
			if *head.ID <= after {
				return nil, nil, fmt.Errorf("replay: collect: Dispatch's event log serves the event %d after the event %d", *head.ID, after)
			}
			after = *head.ID
			if !head.CreatedAt.Before(end) {
				done = true
				continue
			}
			if head.Project == nil || *head.Project != project {
				continue
			}
			if strings.HasPrefix(head.Type, "settings.") {
				if _, err := ParseSettingsEvent(raw); err != nil {
					return nil, nil, fmt.Errorf("replay: collect: %w", err)
				}
				settings = append(settings, bytes.Clone(raw))
			}
			if head.IssueKey != nil {
				keys[*head.IssueKey] = true
			}
		}
		if done {
			return settings, slices.Sorted(maps.Keys(keys)), nil
		}
	}
}

// listProjectIssues is the key of every issue of project, of every status, read from Dispatch's
// listing a page at a time (DispatchReader's IssuesPage, issuesPage issues a page, each page
// Dispatch refuses for now tried again with a growing wait) until it has read as many as the
// listing holds or a page holds none, each key once.
func listProjectIssues(ctx context.Context, d DispatchReader, project string, wait func(context.Context, time.Duration) error) ([]string, error) {
	keys := make(map[string]bool)
	for offset := 0; ; {
		var page []string
		var total int
		err := retry(ctx, wait, retryableDispatch, func() (err error) {
			page, total, err = d.IssuesPage(ctx, project, offset, issuesPage)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("replay: collect: list %s's issues from Dispatch at offset %d: %w", project, offset, err)
		}
		for _, key := range page {
			keys[key] = true
		}
		if offset += len(page); len(page) == 0 || offset >= total {
			return slices.Sorted(maps.Keys(keys)), nil
		}
	}
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

// filesOf is every file paths names, in the order paths gives and, under a directory, every
// regular file beneath it by file name whose name keep keeps, or every one where keep is nil. A
// file paths names itself is read whatever its name.
func filesOf(paths []string, keep func(name string) bool) ([]string, error) {
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("replay: collect: %w", err)
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		err = filepath.WalkDir(path, func(file string, entry fs.DirEntry, err error) error {
			if err == nil && entry.Type().IsRegular() && (keep == nil || keep(entry.Name())) {
				files = append(files, file)
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("replay: collect: %w", err)
		}
	}
	return files, nil
}

// signalInputSuffixes is the ends of the names of the files a directory of a signal's record
// holds that collect reads (signalInputFile): JSON lines or one JSON array, gzipped or not. An
// export's other files beside them, its manifest of their hashes among them, are no record.
var signalInputSuffixes = []string{".jsonl", ".jsonl.gz", ".json", ".json.gz"}

// signalInputFile says whether name is the name of a file collect reads under a directory of a
// signal's record (signalInputSuffixes).
func signalInputFile(name string) bool {
	return slices.ContainsFunc(signalInputSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) })
}

// readProvider is the provider's minutes of file in [from, to), in time order, each read by
// ParseProviderMinute. It refuses a line that does not read, naming the file and the line, and a
// minute the file holds twice.
func readProvider(file string, from, to time.Time) ([]ProviderMinute, error) {
	minutes := []ProviderMinute{}
	seen := make(map[time.Time]bool)
	err := readLogFile(file, func(file string, at int, element []byte) error {
		minute, err := ParseProviderMinute(element)
		if err != nil {
			return fmt.Errorf("replay: collect: %s line %d: %w", file, at, err)
		}
		if minute.Minute.Before(from) || !minute.Minute.Before(to) {
			return nil
		}
		if seen[minute.Minute] {
			return fmt.Errorf("replay: collect: %s holds the minute %s twice", file, stamp(minute.Minute))
		}
		seen[minute.Minute] = true
		minutes = append(minutes, minute)
		return nil
	})
	slices.SortStableFunc(minutes, func(a, b ProviderMinute) int { return a.Minute.Compare(b.Minute) })
	return minutes, err
}

// readLauncherLogs is the launcher's lines of the files and directories paths name (filesOf, a
// directory's JSON files alone: signalInputFile), each read by ParseLauncherEntry, written in
// [from, to), each once, as the window's rows, ordered by their time (two of one time in the order
// read). It refuses an element that is no launcher line, naming the file and its place.
func readLauncherLogs(paths []string, from, to time.Time) ([]LauncherLine, error) {
	files, err := filesOf(paths, signalInputFile)
	if err != nil {
		return nil, err
	}
	lines := []LauncherLine{}
	seen := make(map[LauncherLine]bool)
	for _, file := range files {
		err := readLogFile(file, func(file string, at int, element []byte) error {
			line, err := ParseLauncherEntry(element)
			if err != nil {
				return fmt.Errorf("replay: collect: %s line %d: %w", file, at, err)
			}
			if line.Time.Before(from) || !line.Time.Before(to) || seen[line] {
				return nil
			}
			seen[line] = true
			lines = append(lines, line)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(lines, func(a, b LauncherLine) int { return a.Time.Compare(b.Time) })
	return lines, nil
}

// memorySeries is one sample of one memory series of a pod.
type memorySeries struct {
	minute    time.Time
	pod, kind string
}

// readMemory is the pods' memory of the files and directories paths name (filesOf, a directory's
// JSON files alone: signalInputFile) in [from, to): each pod's bytes in each minute, the sum of its
// series that minute (t25's export gives a pod two, its evictable and its non-evictable memory,
// whose sum is the container's use; a minute with one alone counts that one), ordered by minute
// and then pod. A row of a window's own form, with no memory_type, is the pod's whole use. A series
// a file gives twice in one minute is read once where both give it the same bytes, and refused
// where they differ, as is a row with no minute or pod, or bytes that are no whole number at or
// above zero.
func readMemory(paths []string, from, to time.Time) ([]MemorySample, error) {
	files, err := filesOf(paths, signalInputFile)
	if err != nil {
		return nil, err
	}
	series := make(map[memorySeries]int64)
	for _, file := range files {
		err := readLogFile(file, func(file string, at int, element []byte) error {
			var row struct {
				Minute     *time.Time  `json:"minute"`
				Pod        string      `json:"pod"`
				MemoryType string      `json:"memory_type"`
				Bytes      json.Number `json:"bytes"`
			}
			decoder := json.NewDecoder(bytes.NewReader(element))
			decoder.UseNumber()
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&row); err != nil || row.Minute == nil || row.Pod == "" {
				return fmt.Errorf("replay: collect: %s line %d is no row of a pod's memory with its minute, pod and bytes", file, at)
			}
			count, err := strconv.ParseInt(row.Bytes.String(), 10, 64)
			if whole, ferr := strconv.ParseFloat(row.Bytes.String(), 64); err != nil && ferr == nil && whole == float64(int64(whole)) {
				count, err = int64(whole), nil
			}
			if err != nil || count < 0 {
				return fmt.Errorf("replay: collect: %s line %d counts %q bytes, no whole number at or above zero", file, at, row.Bytes)
			}
			if row.Minute.Before(from) || !row.Minute.Before(to) {
				return nil
			}
			key := memorySeries{minute: row.Minute.UTC(), pod: row.Pod, kind: row.MemoryType}
			if was, read := series[key]; read && was != count {
				return fmt.Errorf("replay: collect: %s line %d gives the %q memory of %s at %s as %d bytes, and another line as %d",
					file, at, row.MemoryType, row.Pod, stamp(key.minute), count, was)
			}
			series[key] = count
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	type podMinute struct {
		minute time.Time
		pod    string
	}
	sums := make(map[podMinute]int64)
	for key, count := range series {
		sums[podMinute{minute: key.minute, pod: key.pod}] += count
	}
	samples := make([]MemorySample, 0, len(sums))
	for key, count := range sums {
		samples = append(samples, MemorySample{Minute: key.minute, Pod: key.pod, Bytes: count})
	}
	slices.SortFunc(samples, func(a, b MemorySample) int {
		return cmp.Or(a.Minute.Compare(b.Minute), strings.Compare(a.Pod, b.Pod))
	})
	return samples, nil
}

// readLogs reads every daemon line of the files and directories paths name (filesOf, every file
// under a directory), and keeps the lines of a kind the trace reads written in [from, to), each
// once, ordered by the time the line carries (two lines of one time in the order read).
func readLogs(paths []string, from, to time.Time) ([]keptLine, logCounts, error) {
	files, err := filesOf(paths, nil)
	if err != nil {
		return nil, logCounts{}, err
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

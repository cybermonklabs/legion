package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
)

// The readers of R11's signals' inputs: each reads one line of a window's input, or of the
// output of the system a collect reads it from, and refuses a line of another form, so a window is
// never written or read that a signal's definition would misread.

// ProviderMinute is one minute of the deployment's record of the model provider, the form t25's
// provider-record script writes (one line a minute): the project's uncached input tokens in the
// minute [Minute, Minute + 1m), the limit in force and its source and scope where an observation of
// it was made at or before the minute (none before the first: the provider's signal has no record
// there), and the refusals the project's own limit gave (Own) and the provider gave (Provider).
// Uncached is kept as written, below zero too: a minute whose cached tokens the provider's series
// counted a minute after their input reads below zero, and the model that reads it decides.
type ProviderMinute struct {
	Minute   time.Time `json:"minute"`
	Uncached int64     `json:"uncached"`
	// Limit is the uncached input tokens a minute the limit in force allows; LimitSource names
	// where its value was observed (the deployment's log of a refusal, or a recorded change in
	// Dispatch), and LimitScope what it limits, as the script writes it: "Claude" for the
	// project's limit over all its Claude models, a model's id for that model's own limit, none
	// where the observation names no one scope.
	Limit       *int64  `json:"limit"`
	LimitSource *string `json:"limit_source"`
	LimitScope  *string `json:"limit_scope"`
	Own         int64   `json:"own"`
	Provider    int64   `json:"provider"`
}

// providerMembers is every member of a provider minute's line, each of which it must hold.
var providerMembers = []string{"minute", "uncached", "limit", "limit_source", "limit_scope", "own", "provider"}

// ParseProviderMinute reads one line of the provider's record. It refuses a line that is not one
// JSON object of exactly providerMembers, a minute that is not a whole minute, a limit with no
// source or a source with no limit, and a count of refusals below zero.
func ParseProviderMinute(line []byte) (ProviderMinute, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(line, &members); err != nil {
		return ProviderMinute{}, fmt.Errorf("a provider minute is no JSON object: %w", err)
	}
	if got := slices.Sorted(maps.Keys(members)); !slices.Equal(got, slices.Sorted(slices.Values(providerMembers))) {
		return ProviderMinute{}, fmt.Errorf("a provider minute holds the members %v, want %v", got, providerMembers)
	}
	var m ProviderMinute
	if err := json.Unmarshal(line, &m); err != nil {
		return ProviderMinute{}, fmt.Errorf("a provider minute does not read: %w", err)
	}
	switch {
	case m.Minute.IsZero() || !m.Minute.Equal(m.Minute.Truncate(time.Minute)):
		return ProviderMinute{}, fmt.Errorf("a provider minute's minute %s is no whole minute", stamp(m.Minute))
	case (m.Limit == nil) != (m.LimitSource == nil):
		return ProviderMinute{}, fmt.Errorf("the provider minute %s names a limit or its source without the other", stamp(m.Minute))
	case m.Own < 0 || m.Provider < 0:
		return ProviderMinute{}, fmt.Errorf("the provider minute %s counts refusals below zero", stamp(m.Minute))
	}
	return m, nil
}

// LauncherLine is one line the launcher of one of the project's issue pods wrote: when, in which
// pod, and its text verbatim. The memory guard's lines among them are the record of its kills
// (ParseMemoryGuard).
type LauncherLine struct {
	Time time.Time `json:"time"`
	Pod  string    `json:"pod"`
	Text string    `json:"text"`
}

// check refuses a launcher line with no time, no pod or no text.
func (l LauncherLine) check() error {
	if l.Time.IsZero() || l.Pod == "" || l.Text == "" {
		return errors.New("a launcher line lacks its time, its pod or its text")
	}
	return nil
}

// ParseLauncherEntry reads one launcher line in any of the forms a collect reads one from: a Cloud
// Logging entry (timestamp, resource.labels.pod_name and textPayload, as `gcloud logging read
// --format=json` writes it), a row of t25's export of the launcher's lines ({timestamp, pod,
// text}), or a window's own line ({time, pod, text}). It refuses an element of none of them, and
// one that lacks its time, its pod or its text.
func ParseLauncherEntry(element []byte) (LauncherLine, error) {
	var entry struct {
		Timestamp   *time.Time `json:"timestamp"`
		Time        *time.Time `json:"time"`
		Pod         *string    `json:"pod"`
		Text        *string    `json:"text"`
		TextPayload *string    `json:"textPayload"`
		Resource    *struct {
			Labels struct {
				PodName *string `json:"pod_name"`
			} `json:"labels"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(element, &entry); err != nil {
		return LauncherLine{}, fmt.Errorf("a launcher line is no JSON object of a launcher line's members: %w", err)
	}
	var line LauncherLine
	switch {
	case entry.TextPayload != nil && entry.Timestamp != nil && entry.Resource != nil && entry.Resource.Labels.PodName != nil:
		line = LauncherLine{Time: *entry.Timestamp, Pod: *entry.Resource.Labels.PodName, Text: *entry.TextPayload}
	case entry.Text != nil && entry.Pod != nil && (entry.Timestamp == nil) != (entry.Time == nil):
		line = LauncherLine{Time: *cmpOrTime(entry.Timestamp, entry.Time), Pod: *entry.Pod, Text: *entry.Text}
	default:
		return LauncherLine{}, fmt.Errorf("%s is no launcher line: neither a log entry with its timestamp, pod and textPayload nor a row with "+
			"its time or timestamp, pod and text", bytes.TrimSpace(element))
	}
	line.Time = line.Time.UTC()
	return line, line.check()
}

// cmpOrTime is the first of a and b that is not nil.
func cmpOrTime(a, b *time.Time) *time.Time {
	if a != nil {
		return a
	}
	return b
}

// MemorySample is one pod's memory in one minute: the bytes of the pod's roles container in use.
type MemorySample struct {
	Minute time.Time `json:"minute"`
	Pod    string    `json:"pod"`
	Bytes  int64     `json:"bytes"`
}

// check refuses a sample with no minute or no pod, and one below zero.
func (s MemorySample) check() error {
	if s.Minute.IsZero() || s.Pod == "" || s.Bytes < 0 {
		return errors.New("a memory sample lacks its minute or its pod, or counts bytes below zero")
	}
	return nil
}

// memoryGuardPrefix is what every line of the launcher's memory guard starts with.
const memoryGuardPrefix = "launcher: memory guard: "

// guardField is where a `key=value` field of a memory guard line starts.
var guardField = regexp.MustCompile(`(?:^| )([a-z_]+)=`)

// MemoryGuardLine is a line of the launcher's memory guard, read: its kind, the words before its
// first field ("ended", "stopped for this rise", "started", "settings", "hour", "off", …), its
// `key=value` fields, and, for a line whose kind is followed by a colon ("off: …", "settings
// refused: …"), the text after it in Detail, and no fields. A quoted value, the executable's name
// the guard writes last, is the text between its quotes, as the guard writes it.
type MemoryGuardLine struct {
	Kind   string
	Detail string
	Fields map[string]string
}

// ParseMemoryGuard reads text, a launcher line's text, as a line of the memory guard's. It answers
// false for a text that is no such line, and refuses one that names no kind, one that names a
// field twice, and an ended line, the record of a kill, without its pid as a number or its role.
func ParseMemoryGuard(text string) (MemoryGuardLine, bool, error) {
	rest, ok := strings.CutPrefix(text, memoryGuardPrefix)
	if !ok {
		return MemoryGuardLine{}, false, nil
	}
	rest = strings.TrimRight(rest, "\r\n")
	matches := guardField.FindAllStringSubmatchIndex(rest, -1)
	head := rest
	if len(matches) > 0 {
		head = rest[:matches[0][0]]
	}
	var line MemoryGuardLine
	if kind, detail, colon := strings.Cut(head, ":"); colon {
		line = MemoryGuardLine{Kind: strings.TrimSpace(kind), Detail: strings.TrimSpace(detail + rest[len(head):])}
	} else {
		line = MemoryGuardLine{Kind: strings.TrimSpace(head), Fields: make(map[string]string, len(matches))}
		for i, match := range matches {
			key, start, end := rest[match[2]:match[3]], match[1], len(rest)
			if i+1 < len(matches) {
				end = matches[i+1][0]
			}
			if _, twice := line.Fields[key]; twice {
				return MemoryGuardLine{}, true, fmt.Errorf("the memory guard line %q names %s twice", text, key)
			}
			if last := strings.LastIndex(rest, `"`); strings.HasPrefix(rest[start:], `"`) && last > start {
				line.Fields[key] = rest[start+1 : last]
				break
			}
			line.Fields[key] = rest[start:end]
		}
	}
	if line.Kind == "" {
		return MemoryGuardLine{}, true, fmt.Errorf("the memory guard line %q names no kind", text)
	}
	if _, numbered := line.PID(); line.Kill() && (!numbered || line.Role() == "") {
		return MemoryGuardLine{}, true, fmt.Errorf("the memory guard's kill line %q names no pid or no role", text)
	}
	return line, true, nil
}

// Kill says whether l is the record of a kill: the guard's line `ended`, which it writes for each
// process it ended.
func (l MemoryGuardLine) Kill() bool { return l.Kind == "ended" }

// PID is the pid l names, and whether it names one as a number.
func (l MemoryGuardLine) PID() (int, bool) {
	pid, err := strconv.Atoi(l.Fields["pid"])
	return pid, err == nil
}

// Role is the role l names ("none" for a process outside every role), "" where it names none.
func (l MemoryGuardLine) Role() string { return l.Fields["role"] }

// The kinds of a pull request's timeline event a window's queue file keeps.
const (
	queueAdded   = "added_to_merge_queue"
	queueRemoved = "removed_from_merge_queue"
	queueMerged  = "merged"
)

// QueueEvent is one line of a window's queue file, read: the pull request, the kind of its
// timeline event (added_to_merge_queue, removed_from_merge_queue or merged), the event's id, the
// repository its url names (owner/name, as GitHub names it), when GitHub recorded it, the login
// of its actor ("" where GitHub names none) and, for a merge, its commit. The repository is the
// url's and never the file's: GitHub answers a renamed repository's former name with the
// repository's own events, so a file collected under the former name holds events whose url names
// the current one.
type QueueEvent struct {
	PullRequest int
	Event       string
	ID          int64
	Repo        string
	At          time.Time
	Actor       string
	Commit      string
}

// queueEventURL is the url GitHub's REST API gives a timeline event,
// <API>/repos/<owner>/<name>/issues/events/<id>, its owner and name: the API at any host and path,
// DefaultGitHubAPI or another a collect-queue reads (QueueConfig.API).
var queueEventURL = regexp.MustCompile(`^https?://[^/?#]+(?:/[^?#]*)?/repos/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/events/[0-9]+$`)

// ParseQueueTimeline is the queue lines of pages, the pages of pull request pullRequest's
// timeline as GitHub's REST API serves them (GET /repos/<owner>/<name>/issues/<n>/timeline): each
// event of the kinds a queue file keeps, in the timeline's order, exactly as GitHub serves it,
// with the pull request's number beside it: {"pullRequest": <n>, "item": <the event>}. It
// refuses a page that is not a JSON array of timeline events, an event with no kind, and a kept
// event ParseQueueLine refuses.
func ParseQueueTimeline(pullRequest int, pages ...[]byte) ([]json.RawMessage, error) {
	if pullRequest <= 0 {
		return nil, fmt.Errorf("replay: %d is no pull request's number", pullRequest)
	}
	lines := []json.RawMessage{}
	for i, page := range pages {
		var items []json.RawMessage
		if err := json.Unmarshal(page, &items); err != nil {
			return nil, fmt.Errorf("replay: page %d of the timeline of #%d is no JSON array of its events: %w", i+1, pullRequest, err)
		}
		for j, item := range items {
			var head struct {
				Event *string `json:"event"`
			}
			if err := json.Unmarshal(item, &head); err != nil || head.Event == nil {
				return nil, fmt.Errorf("replay: event %d of page %d of the timeline of #%d names no kind", j+1, i+1, pullRequest)
			}
			if *head.Event != queueAdded && *head.Event != queueRemoved && *head.Event != queueMerged {
				continue
			}
			var line bytes.Buffer
			fmt.Fprintf(&line, `{"pullRequest":%d,"item":`, pullRequest)
			if err := json.Compact(&line, item); err != nil {
				return nil, err
			}
			line.WriteByte('}')
			if _, err := ParseQueueLine(line.Bytes()); err != nil {
				return nil, err
			}
			lines = append(lines, line.Bytes())
		}
	}
	return lines, nil
}

// ParseQueueLine reads one line of a window's queue file. It refuses a line that is not
// {pullRequest, item} with a pull request's number, an item that is no merge queue event of the
// kinds the file keeps or that has no created_at, an item with no event id, and one whose url names
// no repository (queueEventURL), each refusal saying which.
func ParseQueueLine(line []byte) (QueueEvent, error) {
	var wrapped struct {
		PullRequest *int            `json:"pullRequest"`
		Item        json.RawMessage `json:"item"`
	}
	if err := decodeStrict(line, &wrapped); err != nil || wrapped.PullRequest == nil || *wrapped.PullRequest <= 0 || len(wrapped.Item) == 0 {
		return QueueEvent{}, fmt.Errorf("replay: the queue line %s is no {pullRequest, item} of a pull request's number and its event", line)
	}
	var item struct {
		ID        *int64     `json:"id"`
		URL       *string    `json:"url"`
		Event     *string    `json:"event"`
		CreatedAt *time.Time `json:"created_at"`
		Actor     *struct {
			Login string `json:"login"`
		} `json:"actor"`
		CommitID *string `json:"commit_id"`
	}
	if err := json.Unmarshal(wrapped.Item, &item); err != nil {
		return QueueEvent{}, fmt.Errorf("replay: the queue line of #%d holds no timeline event: %w", *wrapped.PullRequest, err)
	}
	if item.Event == nil || (*item.Event != queueAdded && *item.Event != queueRemoved && *item.Event != queueMerged) || item.CreatedAt == nil {
		return QueueEvent{}, fmt.Errorf("replay: the queue line of #%d holds no merge queue event with its created_at", *wrapped.PullRequest)
	}
	if item.ID == nil || *item.ID <= 0 {
		return QueueEvent{}, fmt.Errorf("replay: the queue line of #%d holds no event id", *wrapped.PullRequest)
	}
	if item.URL == nil {
		return QueueEvent{}, fmt.Errorf("replay: the queue line of #%d holds no url, so it names no repository", *wrapped.PullRequest)
	}
	named := queueEventURL.FindStringSubmatch(*item.URL)
	if named == nil {
		return QueueEvent{}, fmt.Errorf("replay: the url %q of the queue line of #%d names no repository, as <API>/repos/<owner>/<name>/issues/events/<id> does",
			*item.URL, *wrapped.PullRequest)
	}
	event := QueueEvent{PullRequest: *wrapped.PullRequest, Event: *item.Event, ID: *item.ID, Repo: named[1] + "/" + named[2], At: item.CreatedAt.UTC()}
	if item.Actor != nil {
		event.Actor = item.Actor.Login
	}
	if item.CommitID != nil {
		event.Commit = *item.CommitID
	}
	return event, nil
}

// defaultAssigneeEvent is the settings event that moves a project's default assignee.
const defaultAssigneeEvent = "settings.project_default_assignee.updated"

// SettingsEvent is one of a project's settings events of Dispatch's event log, read: its id, its
// place in the project's own log (Seq), its project and type (settings.<what>.updated), who made
// the change, when, and, for settings.project_default_assignee.updated, the person the project's
// default assignee becomes.
type SettingsEvent struct {
	ID, Seq         int64
	Project, Type   string
	Actor           dispatch.Actor
	At              time.Time
	DefaultAssignee string
}

// ParseSettingsEvent reads one settings event as Dispatch's event log serves it (GET
// /api/v1/events/log). It refuses an event that is not one JSON object, that lacks its id, seq,
// project, type, actor or created_at, whose type is no settings type, and a move of the default
// assignee that names no assignee.
func ParseSettingsEvent(event []byte) (SettingsEvent, error) {
	var e struct {
		ID        *int64     `json:"id"`
		Seq       *int64     `json:"seq"`
		Project   *string    `json:"project"`
		Type      *string    `json:"type"`
		CreatedAt *time.Time `json:"created_at"`
		Actor     *struct {
			Kind *string `json:"kind"`
			ID   *string `json:"id"`
		} `json:"actor"`
		Payload struct {
			Assignee *string `json:"assignee"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(event, &e); err != nil {
		return SettingsEvent{}, fmt.Errorf("replay: a settings event is no JSON object of an event's members: %w", err)
	}
	if e.ID == nil || e.Seq == nil || e.Project == nil || e.Type == nil || e.CreatedAt == nil || e.Actor == nil || e.Actor.Kind == nil || e.Actor.ID == nil {
		return SettingsEvent{}, fmt.Errorf("replay: the settings event %s lacks its id, seq, project, type, actor or created_at", event)
	}
	if !strings.HasPrefix(*e.Type, "settings.") {
		return SettingsEvent{}, fmt.Errorf("replay: the event %d is of the type %s, no settings type", *e.ID, *e.Type)
	}
	parsed := SettingsEvent{ID: *e.ID, Seq: *e.Seq, Project: *e.Project, Type: *e.Type, Actor: dispatch.Actor{Kind: *e.Actor.Kind, ID: *e.Actor.ID},
		At: e.CreatedAt.UTC()}
	if parsed.Type == defaultAssigneeEvent {
		if e.Payload.Assignee == nil || *e.Payload.Assignee == "" {
			return SettingsEvent{}, fmt.Errorf("replay: the event %d moves the project's default assignee and names no assignee", parsed.ID)
		}
		parsed.DefaultAssignee = *e.Payload.Assignee
	}
	return parsed, nil
}

// attributionEvent is the issue event Dispatch appends when a pull request's issue changes.
const attributionEvent = "issue.pull_request_attribution_changed"

// DispatchAttribution is an issue.pull_request_attribution_changed event's move of a pull
// request: its repository and number, the issue it left and the issue it reached ("" for none,
// never both), and the source that decided it.
type DispatchAttribution struct {
	Repo     string
	Number   int
	From, To string
	Source   string
}

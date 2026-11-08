package restatex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Admin is the Restate server's admin API: the endpoint that registers deployments,
// answers SQL over the server's own tables, and pauses, resumes and kills invocations.
type Admin struct {
	base   string
	client *http.Client
}

// NewAdmin addresses the admin API at a loopback HOST:PORT.
func NewAdmin(address string) *Admin {
	return &Admin{
		base:   "http://" + address,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Version is the server's version, which is also the cheapest proof that the admin API
// is up. Only the version string is returned: the rest of that response names the
// address the node advertises, which these experiments never print.
func (a *Admin) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := a.do(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// Deployment is one registered endpoint: the deployment id invocations are pinned to,
// and the services discovered behind it with the revision each one reached.
type Deployment struct {
	ID       string
	Services []ServiceRevision
}

// ServiceRevision is one service a deployment serves. Restate bumps a service's
// revision every time a new deployment serving it is registered.
type ServiceRevision struct {
	Name     string `json:"name"`
	Revision int    `json:"revision"`
}

// Register points the server at a service endpoint and makes it call it to discover
// what is behind it. It is the step that must happen before anything can be invoked:
// an unregistered endpoint is, to Restate, not there.
//
// force overwrites a deployment already registered at the same URI, which is what a
// redeploy to an unchanged address needs.
func (a *Admin) Register(ctx context.Context, uri string, force bool) (Deployment, error) {
	body := map[string]any{"uri": uri, "force": force}
	var out struct {
		ID       string            `json:"id"`
		Services []ServiceRevision `json:"services"`
	}
	if err := a.do(ctx, http.MethodPost, "/deployments", body, &out); err != nil {
		return Deployment{}, fmt.Errorf("register deployment %s: %w", uri, err)
	}
	return Deployment{ID: out.ID, Services: out.Services}, nil
}

// Query runs SQL against the server's introspection tables and returns the rows as
// decoded JSON. It is how these experiments read an invocation's state: restate-server
// 1.8.1's admin API has no GET /invocations, only the per-invocation operations, so
// every status reading here comes from the sys_* views through this endpoint.
func (a *Admin) Query(ctx context.Context, sql string) ([]map[string]any, error) {
	var out struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := a.do(ctx, http.MethodPost, "/query", map[string]any{"query": sql}, &out); err != nil {
		return nil, fmt.Errorf("query %q: %w", sql, err)
	}
	return out.Rows, nil
}

// Invocation is the part of one sys_invocation row the experiments read.
type Invocation struct {
	// Found is false when no row with that id exists, which is what the server shows
	// once an invocation's retention has passed.
	Found bool
	ID    string
	// Target is the service and handler, and Status the invocation's state: one of
	// pending, scheduled, ready, running, backing-off, suspended, paused, succeeded,
	// failed, cancelled or killed.
	Target string
	Status string
	// PinnedDeployment is the deployment this invocation is bound to. Restate pins an
	// invocation to a deployment on its first attempt and keeps it there.
	PinnedDeployment string
	// InFlight is whether the server currently has an attempt of this invocation open
	// against a service endpoint.
	InFlight bool
	// RetryCount is how many attempts have failed, NextRetryAt when the next one is
	// due, and LastFailure what the last one said.
	RetryCount  int64
	NextRetryAt string
	LastFailure string
	// LastFailureErrorCode is the code the service or the server attached to that
	// failure, which is how a journal mismatch is told apart from an ordinary error.
	LastFailureErrorCode string
	LastFailureAt        string
	// JournalSize is how many entries the journal holds, and Retention how long the
	// completed invocation will be kept.
	JournalSize         int64
	CompletionRetention string
	JournalRetention    string
	// SuspendedWaitingForSignals is what a suspended invocation is waiting on, which
	// for an awakeable is that awakeable's signal.
	SuspendedWaitingForSignals string
	// CompletionResult and CompletionFailure are what a terminal invocation ended as.
	CompletionResult  string
	CompletionFailure string
}

// invocationQuery is the projection and the join every invocation reading uses, so
// every fact about an invocation comes from one shape of row. Whether the server has
// an attempt open against a service endpoint lives in sys_invocation_state rather than
// in sys_invocation, which is why the two are joined.
const invocationQuery = `SELECT i.id, i.target, i.status, i.pinned_deployment_id,
	coalesce(s.in_flight, false) AS in_flight, i.retry_count, i.next_retry_at,
	i.last_failure, i.last_failure_error_code, i.last_start_at, i.journal_size,
	i.completion_retention, i.journal_retention, i.suspended_waiting_for_signals,
	i.completion_result, i.completion_failure
	FROM sys_invocation i LEFT JOIN sys_invocation_state s ON i.id = s.id`

// Invocation reads one invocation's row.
func (a *Admin) Invocation(ctx context.Context, id string) (Invocation, error) {
	rows, err := a.Query(ctx, fmt.Sprintf("%s WHERE i.id = '%s'", invocationQuery, id))
	if err != nil {
		return Invocation{}, err
	}
	if len(rows) == 0 {
		return Invocation{ID: id}, nil
	}
	return decodeInvocation(rows[0]), nil
}

// Invocations lists the invocations a WHERE clause selects, which is how an operator
// would find what has stopped and needs a person.
func (a *Admin) Invocations(ctx context.Context, where string) ([]Invocation, error) {
	rows, err := a.Query(ctx, fmt.Sprintf("%s WHERE %s ORDER BY i.id", invocationQuery, where))
	if err != nil {
		return nil, err
	}
	out := make([]Invocation, 0, len(rows))
	for _, row := range rows {
		out = append(out, decodeInvocation(row))
	}
	return out, nil
}

func decodeInvocation(row map[string]any) Invocation {
	return Invocation{
		Found:                      true,
		ID:                         str(row["id"]),
		Target:                     str(row["target"]),
		Status:                     str(row["status"]),
		PinnedDeployment:           str(row["pinned_deployment_id"]),
		InFlight:                   row["in_flight"] == true,
		RetryCount:                 num(row["retry_count"]),
		NextRetryAt:                str(row["next_retry_at"]),
		LastFailure:                str(row["last_failure"]),
		LastFailureErrorCode:       str(row["last_failure_error_code"]),
		LastFailureAt:              str(row["last_start_at"]),
		JournalSize:                num(row["journal_size"]),
		CompletionRetention:        str(row["completion_retention"]),
		JournalRetention:           str(row["journal_retention"]),
		SuspendedWaitingForSignals: str(row["suspended_waiting_for_signals"]),
		CompletionResult:           str(row["completion_result"]),
		CompletionFailure:          str(row["completion_failure"]),
	}
}

// String is the one-line form an experiment prints as a fact. Fields the row does not
// hold are left out, so a reading says what the server had and nothing more.
func (i Invocation) String() string {
	if !i.Found {
		return "no row in sys_invocation"
	}
	parts := []string{"status=" + i.Status, fmt.Sprintf("in_flight=%t", i.InFlight)}
	if i.PinnedDeployment != "" {
		parts = append(parts, "pinned_deployment_id="+i.PinnedDeployment)
	}
	parts = append(parts, fmt.Sprintf("retry_count=%d journal_size=%d", i.RetryCount, i.JournalSize))
	if i.NextRetryAt != "" {
		parts = append(parts, "next_retry_at="+i.NextRetryAt)
	}
	if i.SuspendedWaitingForSignals != "" {
		parts = append(parts, "suspended_waiting_for_signals="+i.SuspendedWaitingForSignals)
	}
	if i.CompletionResult != "" {
		parts = append(parts, "completion_result="+i.CompletionResult)
	}
	if i.CompletionFailure != "" {
		parts = append(parts, "completion_failure="+Message(i.CompletionFailure))
	}
	if i.LastFailure != "" {
		parts = append(parts, "last_failure="+Message(i.LastFailure))
	}
	return strings.Join(parts, " ")
}

// Terminal reports whether the invocation has reached a state it cannot leave on its
// own. restate-server 1.8.1 has one terminal status, `completed`, and says in
// completion_result whether it completed with a value or a failure; an invocation that
// was killed or cancelled is `completed` with a failure. A paused invocation is not
// terminal: it is waiting for a person.
func (i Invocation) Terminal() bool {
	return i.Status == "completed"
}

// Succeeded reports whether the invocation finished with a value rather than a failure.
func (i Invocation) Succeeded() bool {
	return i.Terminal() && i.CompletionResult == "success"
}

// WaitForStatus blocks until the invocation reaches one of the wanted statuses, and
// returns the last reading it took whether or not it got there.
func (a *Admin) WaitForStatus(ctx context.Context, id string, timeout time.Duration, want ...string) (Invocation, error) {
	deadline := time.Now().Add(timeout)
	var last Invocation
	for {
		var err error
		last, err = a.Invocation(ctx, id)
		if err != nil {
			return last, err
		}
		for _, status := range want {
			if last.Found && last.Status == status {
				return last, nil
			}
		}
		if !time.Now().Before(deadline) {
			return last, fmt.Errorf("invocation %s was %q, not one of %v, after %s",
				id, last.Status, want, timeout)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// JournalEntry is one row of an invocation's journal as the server holds it.
type JournalEntry struct {
	Index     int64
	Type      string
	Name      string
	Completed bool
}

// String is the one-line form an experiment prints. The completed flag is shown only
// when it is set: in journal version 2 a command's result arrives as a notification
// entry of its own rather than as a flag on the command, so a false flag on a command
// says nothing.
func (e JournalEntry) String() string {
	name := e.Name
	if name == "" {
		name = "-"
	}
	if e.Completed {
		return fmt.Sprintf("%d:%s(%s) completed", e.Index, e.Type, name)
	}
	return fmt.Sprintf("%d:%s(%s)", e.Index, e.Type, name)
}

// Journal is everything the server recorded for an invocation, in order. It is what
// survives a process dying, so reading it after a kill is how an experiment shows
// whether the journal survived.
func (a *Admin) Journal(ctx context.Context, id string) ([]JournalEntry, error) {
	rows, err := a.Query(ctx, fmt.Sprintf(
		"SELECT index, entry_type, name, completed FROM sys_journal WHERE id = '%s' ORDER BY index", id))
	if err != nil {
		return nil, err
	}
	out := make([]JournalEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, JournalEntry{
			Index:     num(row["index"]),
			Type:      str(row["entry_type"]),
			Name:      str(row["name"]),
			Completed: row["completed"] == true,
		})
	}
	return out, nil
}

// JournalEvent is one thing that happened to an invocation without changing its
// journal: a failed attempt, a pause. It matters because once an invocation is paused
// its sys_invocation_state row is gone and with it last_failure, so the only place
// left that says why it stopped is here.
type JournalEvent struct {
	Index   int64
	Type    string
	Code    int64
	Message string
}

// String is the one-line form an experiment prints.
func (e JournalEvent) String() string {
	if e.Message == "" {
		return fmt.Sprintf("after %d: %s", e.Index, e.Type)
	}
	return fmt.Sprintf("after %d: %s [%d] %s", e.Index, e.Type, e.Code, Message(e.Message))
}

// JournalEvents is everything that happened to an invocation beside its journal.
func (a *Admin) JournalEvents(ctx context.Context, id string) ([]JournalEvent, error) {
	rows, err := a.Query(ctx, fmt.Sprintf(
		"SELECT after_journal_entry_index, event_type, event_json FROM sys_journal_events WHERE id = '%s' ORDER BY after_journal_entry_index", id))
	if err != nil {
		return nil, err
	}
	out := make([]JournalEvent, 0, len(rows))
	for _, row := range rows {
		event := JournalEvent{
			Index: num(row["after_journal_entry_index"]),
			Type:  str(row["event_type"]),
		}
		// The event's JSON also carries a stack trace of the handler's process,
		// which is pages long and says nothing about Restate: only the code and the
		// message are taken.
		var detail struct {
			ErrorCode    int64  `json:"error_code"`
			ErrorMessage string `json:"error_message"`
		}
		if err := json.Unmarshal([]byte(str(row["event_json"])), &detail); err == nil {
			event.Code = detail.ErrorCode
			event.Message = detail.ErrorMessage
		}
		out = append(out, event)
	}
	return out, nil
}

// JournalEventsSummary is the one-line form of an invocation's events.
func JournalEventsSummary(events []JournalEvent) string {
	if len(events) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(events))
	for _, event := range events {
		parts = append(parts, event.String())
	}
	return strings.Join(parts, "; ")
}

// JournalSummary is the one-line form of a journal, for a fact line.
func JournalSummary(entries []JournalEntry) string {
	if len(entries) == 0 {
		return "empty"
	}
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, entry.String())
	}
	return strings.Join(parts, ", ")
}

// ServiceRevisions is what the server currently serves each service from: its revision
// and the deployment the next new invocation will be sent to.
func (a *Admin) ServiceRevisions(ctx context.Context, name string) (revision int64, deployment string, err error) {
	rows, err := a.Query(ctx, fmt.Sprintf(
		"SELECT revision, deployment_id FROM sys_service WHERE name = '%s'", name))
	if err != nil || len(rows) == 0 {
		return 0, "", err
	}
	return num(rows[0]["revision"]), str(rows[0]["deployment_id"]), nil
}

// DeploymentEndpoints is every registered deployment, as id and endpoint, sorted by
// id so a fact line reads the same on every run.
func (a *Admin) DeploymentEndpoints(ctx context.Context) ([]string, error) {
	rows, err := a.Query(ctx, "SELECT id, endpoint FROM sys_deployment")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmt.Sprintf("%s=%s", str(row["id"]), str(row["endpoint"])))
	}
	sort.Strings(out)
	return out, nil
}

// Pause, Resume, Kill and Cancel are the admin API's operations on a single
// invocation. Each returns the HTTP status the server answered with, because what the
// server refuses is as much a measurement as what it accepts: pausing an invocation
// that is not running answers 409, and the experiments print that rather than hide it.
func (a *Admin) Pause(ctx context.Context, id string) (string, error) {
	return a.invocationOp(ctx, id, "pause", "")
}

// Resume restarts a paused or suspended invocation. deployment is the optional
// `deployment` query parameter: "keep", "latest" or a deployment id.
func (a *Admin) Resume(ctx context.Context, id, deployment string) (string, error) {
	query := ""
	if deployment != "" {
		query = "?deployment=" + deployment
	}
	return a.invocationOp(ctx, id, "resume", query)
}

// Kill ends an invocation at once, without letting its handler finish.
func (a *Admin) Kill(ctx context.Context, id string) (string, error) {
	return a.invocationOp(ctx, id, "kill", "")
}

func (a *Admin) invocationOp(ctx context.Context, id, op, query string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/invocations/%s/%s%s", a.base, id, op, query), nil)
	if err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s invocation %s: %w", op, id, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	status := resp.Status
	if len(bytes.TrimSpace(body)) > 0 {
		status += " " + oneLine(string(body))
	}
	return status, nil
}

// do is one admin API call with a JSON request and a JSON reply.
func (a *Admin) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, oneLine(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s %s: decode %s: %w", method, path, oneLine(string(payload)), err)
	}
	return nil
}

// str is a JSON value as a string, and the empty string when the column was null.
func str(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// num is a JSON number as an integer, and zero when the column was null.
func num(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	default:
		return 0
	}
}

// oneLine folds a multi-line server message onto one line, so a fact stays one line.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// maxMessage is how much of a failure message a fact line carries.
const maxMessage = 600

// Message is a failure the server recorded, fit for one fact line: folded onto one
// line, with the goroutine dump the Go SDK attaches to every error cut off, and capped.
// What is cut is always the tail, so the sentence the handler or the server wrote
// survives whole. A message that was cut ends in an ellipsis.
func Message(text string) string {
	line := oneLine(text)
	if i := strings.Index(line, "goroutine "); i > 0 {
		line = strings.TrimSpace(line[:i]) + " [...]"
	}
	if len(line) > maxMessage {
		line = strings.TrimSpace(line[:maxMessage]) + " [...]"
	}
	return line
}

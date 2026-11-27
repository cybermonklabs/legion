package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legionbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// One arm of R8 (spec Design, "How R8 is measured", steps 3 to 5) against a server booted on a
// database legionbench loaded at ten times the run volume, with the stand-in as INFRA's daemon:
//
//  1. It waits for the server to catch up (waitConsumers): a good read of the stand-in and, where
//     the server runs the Legion facts consumer (-facts, this head), INFRA read whole under the
//     running build, the consumer's cursors at the feeds' heads and the outbox drained.
//  2. It opens one `legion` view of the project with this head's delivery-live-probe.ts, which
//     writes a line for each frame Dispatch sends, with the time it arrived, and waits for its
//     snapshot.
//  3. For -duration (10 minutes) it appends issue events through the API at -rate (0.26 a second,
//     ten times 812,386 events in 365 days): an issue.updated of an issue the board shows, its
//     title edited by a session of the Legion role the issue's phase belongs to, so the view wakes
//     at both builds and the event is a new input of the issue's facts at this head
//     (appendEvents). It records the moment it sent each one. With -facts it reads, every 100 ms,
//     the updated_at of the stored row of each issue it edits and of each issue above it, and
//     records each move: the issue_facts changes the events cause.
//  4. Once the window and a grace for the last pushes have passed, it stops the probe and judges:
//     for each event the first push after it, within 2 s; for each facts change the first push that
//     carries its entry (the delta's `maps.issues`), within 2 s of the row's updated_at; and the
//     server's `dispatch: delivery live recompute view=legion` lines the window logged, counted by
//     cause with the median and the p90 of elapsed_ms, which fail under 50 recomputes. It reads the
//     board's body, raw and under gzip at its default level, which fails over 300 KB with -facts.
//  5. With -facts it takes the board's read of the stored facts from Postgres's statement log
//     (captureFactsRead) and writes it to <out>/facts-read.json, the statement the facts read's
//     measurement (TestFarmBoardFactsRead) explains.
//
// It writes the arm's summary to <out>/<arm>-summary.json, which compare judges against the other
// arm's.

// firstPayloadBound is the most the board's body may take compressed: 300 KB (root R14's first
// payload), as the board's payload test holds it.
const firstPayloadBound = 300 * 1024

// pushBound is root R14's bound on a recompute after a change reaching the open view.
const pushBound = 2 * time.Second

// minimumRecomputes is the fewest recomputes an arm's window must log for its median to judge.
const minimumRecomputes = 50

// driveOptions is the drive's flags.
type driveOptions struct {
	arm, url, tokenFile, database, project string
	probeDir, bun, serverLog, pgLog, out   string
	facts                                  bool
	rate                                   float64
	duration, grace, waitTimeout           time.Duration
	seed                                   uint64
}

// summary is one arm's record, which compare reads.
type summary struct {
	Arm             string           `json:"arm"`
	Build           string           `json:"build"`
	Recomputes      []recompute      `json:"recomputes"`
	MedianMs        float64          `json:"median_ms"`
	P90Ms           float64          `json:"p90_ms"`
	Events          int              `json:"events"`
	LongestEventGap float64          `json:"longest_event_gap_ms"`
	FactsChanges    int              `json:"facts_changes"`
	LongestFactsGap float64          `json:"longest_facts_gap_ms"`
	BodyBytes       int              `json:"body_bytes"`
	BodyGzipBytes   int              `json:"body_gzip_bytes"`
	Cards           int              `json:"cards"`
	ByCause         map[string]stats `json:"by_cause"`
}

// stats is a count with the median and the p90 of its times.
type stats struct {
	Count    int     `json:"count"`
	MedianMs float64 `json:"median_ms"`
	P90Ms    float64 `json:"p90_ms"`
}

// recompute is one `dispatch: delivery live recompute view=legion` line.
type recompute struct {
	Cause     string  `json:"cause"`
	ElapsedMs float64 `json:"elapsed_ms"`
}

func drive(args []string) error {
	flags := flag.NewFlagSet("drive", flag.ExitOnError)
	var o driveOptions
	flags.StringVar(&o.arm, "arm", "", "the arm's name, main or head (required)")
	flags.StringVar(&o.url, "url", "", "the server's base URL (required)")
	flags.StringVar(&o.tokenFile, "token-file", "", "a file holding the server's agent token (DISPATCH_AGENT_TOKEN) (required)")
	flags.StringVar(&o.database, "database", "", "the server's database, as a superuser (required)")
	flags.StringVar(&o.project, "project", legionbench.Project, "the project whose legion view is driven")
	flags.StringVar(&o.probeDir, "probe-dir", "", "this head's packages/dispatch, which holds e2e/delivery-live-probe.ts (required)")
	flags.StringVar(&o.bun, "bun", "bun", "the bun that runs the probe")
	flags.StringVar(&o.serverLog, "server-log", "", "the server's stderr, which holds its recompute lines (required)")
	flags.StringVar(&o.pgLog, "pg-log", "", "Postgres's jsonlog file, which the facts read is taken from (required with -facts)")
	flags.StringVar(&o.out, "out", "", "the directory the arm's records go to (required)")
	flags.BoolVar(&o.facts, "facts", false, "the server runs the Legion facts consumer: wait for its whole read, and judge the facts changes, the body and the facts read")
	flags.Float64Var(&o.rate, "rate", 0.26, "issue events a second")
	flags.DurationVar(&o.duration, "duration", 10*time.Minute, "how long the events are appended for")
	flags.DurationVar(&o.grace, "grace", 5*time.Second, "how long after the last event the probe keeps listening")
	flags.DurationVar(&o.waitTimeout, "wait-timeout", time.Hour, "how long the server may take to catch up")
	flags.Uint64Var(&o.seed, "seed", 5577, "the seed of the order the events visit the cards in")
	if err := flags.Parse(args); err != nil {
		return err
	}
	for name, value := range map[string]string{"arm": o.arm, "url": o.url, "token-file": o.tokenFile, "database": o.database,
		"probe-dir": o.probeDir, "server-log": o.serverLog, "out": o.out} {
		if value == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	if o.facts && o.pgLog == "" {
		return errors.New("-facts needs -pg-log, the statement log the facts read is taken from")
	}
	if o.rate <= 0 || o.duration <= 0 {
		return errors.New("-rate and -duration are above zero")
	}
	tokenBytes, err := os.ReadFile(o.tokenFile)
	if err != nil {
		return fmt.Errorf("read -token-file: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return fmt.Errorf("-token-file %s holds no token", o.tokenFile)
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	r := &report{prefix: "r8 arm=" + o.arm}
	database, err := store.Open(ctx, o.database)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer database.Pool.Close()
	pool := database.Pool
	build, err := serverBuild(ctx, o.url)
	if err != nil {
		return err
	}
	r.result("build=%s project=%s rate=%g duration=%s facts=%t", build, o.project, o.rate, o.duration, o.facts)

	// 1. The catch-up.
	if err := waitConsumers(ctx, r, pool, o.project, build, o.facts, o.waitTimeout); err != nil {
		r.fail("catchup: %v", err)
		return fmt.Errorf("the server did not catch up, so no event is appended")
	}

	// 2. The view.
	probeLog := filepath.Join(o.out, o.arm+"-probe.jsonl")
	probe, err := startProbe(ctx, o, probeLog)
	if err != nil {
		return err
	}
	defer probe.stop()
	if err := waitForSnapshot(ctx, probe, probeLog, 2*time.Minute); err != nil {
		r.fail("probe: %v", err)
		return err
	}
	r.result("probe opened the view %s", legionView(o.project))

	// 3. The window.
	cards, err := readCards(ctx, o, token)
	if err != nil {
		return err
	}
	if len(cards) == 0 {
		r.fail("the board shows no card, so no event can be appended on an issue it shows")
		return errors.New("no card to append an event on")
	}
	r.result("board cards=%d", len(cards))
	parents, err := readParents(ctx, pool, o.project, cards)
	if err != nil {
		return err
	}
	order := slices.Clone(cards)
	rng := rand.New(rand.NewPCG(o.seed, 5577))
	rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	total := int(o.rate * o.duration.Seconds())
	watched := map[string]bool{}
	for index := range total {
		key := order[index%len(order)].Key
		watched[key] = true
		if parent := parents[key]; parent != "" {
			watched[parent] = true
		}
	}
	logStart, err := fileSize(o.serverLog)
	if err != nil {
		return err
	}
	windowStart := time.Now()
	var changes []factsChange
	var watcher sync.WaitGroup
	watchCtx, stopWatch := context.WithCancel(ctx)
	if o.facts {
		watcher.Go(func() {
			changes = watchFacts(watchCtx, pool, o.project, slices.Sorted(maps.Keys(watched)), windowStart)
		})
	}
	events := appendEvents(ctx, r, o, token, order, total, windowStart)
	select {
	case <-ctx.Done():
	case <-time.After(o.grace):
	}
	stopWatch()
	watcher.Wait()
	windowEnd := time.Now()
	logEnd, err := fileSize(o.serverLog)
	if err != nil {
		return err
	}
	probe.stop()
	r.result("window start=%s end=%s events=%d", windowStart.UTC().Format(time.RFC3339Nano), windowEnd.UTC().Format(time.RFC3339Nano), len(events))

	// 4. The judgements.
	frames, err := readFrames(probeLog)
	if err != nil {
		return err
	}
	sum := summary{Arm: o.arm, Build: build, Events: len(events), Cards: len(cards)}
	sum.LongestEventGap = judgeEvents(r, events, frames)
	if o.facts {
		sum.FactsChanges = len(changes)
		sum.LongestFactsGap = judgeFactsChanges(r, changes, frames)
	}
	lines, err := readRecomputes(o.serverLog, logStart, logEnd)
	if err != nil {
		return err
	}
	sum.Recomputes = lines
	sum.ByCause, sum.MedianMs, sum.P90Ms = recomputeStats(lines)
	for _, cause := range slices.Sorted(maps.Keys(sum.ByCause)) {
		one := sum.ByCause[cause]
		r.result("recomputes cause=%s count=%d median_ms=%.1f p90_ms=%.1f", cause, one.Count, one.MedianMs, one.P90Ms)
	}
	r.result("recomputes count=%d median_ms=%.1f p90_ms=%.1f", len(lines), sum.MedianMs, sum.P90Ms)
	if len(lines) < minimumRecomputes {
		r.fail("the window logged %d recomputes of the legion view, fewer than %d", len(lines), minimumRecomputes)
	}
	body, err := readBoard(ctx, o, token)
	if err != nil {
		return err
	}
	sum.BodyBytes, sum.BodyGzipBytes = len(body), gzipped(body)
	r.result("board_body raw_bytes=%d gzip_bytes=%d (%.1f KB) bound_bytes=%d", sum.BodyBytes, sum.BodyGzipBytes,
		float64(sum.BodyGzipBytes)/1024, firstPayloadBound)
	if o.facts && sum.BodyGzipBytes > firstPayloadBound {
		r.fail("the board's body is %d bytes compressed, above the %d byte first-payload bound", sum.BodyGzipBytes, firstPayloadBound)
	}

	// 5. The facts read.
	if o.facts {
		read, err := captureFactsRead(ctx, pool, o.pgLog, func() error {
			_, err := readBoard(ctx, o, token)
			return err
		})
		if err != nil {
			r.fail("facts_read: %v", err)
		} else {
			encoded, _ := json.MarshalIndent(read, "", "  ")
			if err := os.WriteFile(filepath.Join(o.out, "facts-read.json"), encoded, 0o644); err != nil {
				return err
			}
			r.result("facts_read captured params=%d sql_bytes=%d", len(read.Params), len(read.SQL))
		}
	}
	encoded, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.out, o.arm+"-summary.json"), encoded, 0o644); err != nil {
		return err
	}
	if r.failed > 0 {
		return fmt.Errorf("%d FAIL lines", r.failed)
	}
	r.result("drive: no FAIL line")
	return nil
}

// legionView is the probe's --view of project's Legion board.
func legionView(project string) string {
	return fmt.Sprintf(`{"kind":"legion","project":%q}`, project)
}

// probeProcess is the running probe.
type probeProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

// startProbe starts this head's delivery-live-probe.ts on one legion view of the project, its
// frame lines written to path.
func startProbe(ctx context.Context, o driveOptions, path string) (*probeProcess, error) {
	out, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	stderr, err := os.Create(strings.TrimSuffix(path, ".jsonl") + ".stderr")
	if err != nil {
		return nil, err
	}
	minutes := int((o.waitTimeout + o.duration + 30*time.Minute).Minutes())
	cmd := exec.Command(o.bun, "e2e/delivery-live-probe.ts", "--url", o.url, "--token-file", o.tokenFile,
		"--view", legionView(o.project), "--minutes", strconv.Itoa(minutes))
	cmd.Dir = o.probeDir
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the probe: %w", err)
	}
	probe := &probeProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		_ = out.Close()
		_ = stderr.Close()
		close(probe.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			probe.stop()
		case <-probe.done:
		}
	}()
	return probe, nil
}

// stop ends the probe's watch with SIGTERM, which ends it cleanly, and waits for it to exit.
func (p *probeProcess) stop() {
	p.once.Do(func() {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
}

// waitForSnapshot waits until the probe has written its first snapshot line.
func waitForSnapshot(ctx context.Context, probe *probeProcess, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		frames, err := readFrames(path)
		if err != nil {
			return err
		}
		for _, frame := range frames {
			switch frame.Type {
			case "snapshot":
				return nil
			case "error":
				return fmt.Errorf("Dispatch refused the view: %s %s", frame.Code, frame.Message)
			}
		}
		select {
		case <-probe.done:
			return fmt.Errorf("the probe exited before a snapshot arrived; see %s", strings.TrimSuffix(path, ".jsonl")+".stderr")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no snapshot arrived within %s", timeout)
		}
	}
}

// frame is one line the probe wrote: when it arrived, its type, and for a delta the issue keys of
// its `issues` map.
type frame struct {
	At      time.Time       `json:"-"`
	RawAt   string          `json:"at"`
	Type    string          `json:"type"`
	Maps    json.RawMessage `json:"maps"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	issues  []string
}

// pushed is whether the frame is a push of the view's body: a snapshot or a delta.
func (f frame) pushed() bool { return f.Type == "snapshot" || f.Type == "delta" }

// readFrames reads every whole line the probe has written.
func readFrames(path string) ([]frame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var frames []frame
	for line := range bytes.Lines(data) {
		if !bytes.HasSuffix(line, []byte("\n")) {
			break
		}
		var one frame
		if err := json.Unmarshal(line, &one); err != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, one.RawAt)
		if err != nil {
			continue
		}
		one.At = at
		if len(one.Maps) > 0 && one.Type == "delta" {
			var maps map[string]json.RawMessage
			if json.Unmarshal(one.Maps, &maps) == nil {
				_ = json.Unmarshal(maps["issues"], &one.issues)
			}
		}
		frames = append(frames, one)
	}
	return frames, nil
}

// card is one card of the board: its issue, its title and its phase.
type card struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Phase string `json:"phase"`
}

// readBoard is the board's body as GET answers it.
func readBoard(ctx context.Context, o driveOptions, token string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url+"/api/v1/projects/"+o.project+"/legion/board", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET the board: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read the board: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET the board answered %d: %s", response.StatusCode, body)
	}
	return body, nil
}

// readCards is every card the board shows, in its columns' order.
func readCards(ctx context.Context, o driveOptions, token string) ([]card, error) {
	body, err := readBoard(ctx, o, token)
	if err != nil {
		return nil, err
	}
	var board struct {
		Columns []struct {
			Cards []card `json:"cards"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(body, &board); err != nil {
		return nil, fmt.Errorf("decode the board: %w", err)
	}
	var cards []card
	for _, column := range board.Columns {
		cards = append(cards, column.Cards...)
	}
	return cards, nil
}

// readParents is the parent of each card's issue that has one.
func readParents(ctx context.Context, pool *store.Pool, project string, cards []card) (map[string]string, error) {
	keys := make([]string, len(cards))
	for i, one := range cards {
		keys[i] = one.Key
	}
	rows, err := pool.Query(ctx, `select key, parent_key from issues where project_key = $1 and key = any($2) and parent_key is not null`, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read the cards' parents: %w", err)
	}
	defer rows.Close()
	parents := map[string]string{}
	for rows.Next() {
		var key, parent string
		if err := rows.Scan(&key, &parent); err != nil {
			return nil, err
		}
		parents[key] = parent
	}
	return parents, rows.Err()
}

// phaseRoles is the Legion role whose session works an issue in each phase the stand-in reports.
var phaseRoles = map[string]string{
	legion.PhaseAdmitted: "architect", legion.PhasePlanning: "planner", legion.PhaseImplementing: "implementer",
	legion.PhaseTesting: "tester", legion.PhaseReviewing: "reviewer", legion.PhaseMerging: "merger",
}

// appended is one event the drive appended: its issue, the moment it sent the request, the moment
// the answer came, and the status it answered.
type appended struct {
	Key      string    `json:"key"`
	Sent     time.Time `json:"sent"`
	Answered time.Time `json:"answered"`
	Status   int       `json:"status"`
}

// appendEvents appends total issue events, one each 1/rate seconds from start, on the cards in
// order, round and round, and answers each with its times. Each is an issue.updated: a PATCH of the
// card's title, which the board shows, so the live hub recomputes the view at main's tip as at this
// head (an issue.updated of an issue a view names marks it, where a comment marks no view) and the
// recompute has a change to push. Each is written by a session of its own, titled as the Legion
// role the card's phase belongs to works the issue (`Legion <role> · <KEY>`): the facts read each
// session's first event of an issue as a role event (metrics' readRoleEvents), so the edit is a new
// input of the issue's facts and moves its stored row at this head; a second event by a session
// already in the history would move nothing. A write the server refuses is a FAIL line, and is no
// event of the judgement.
func appendEvents(ctx context.Context, r *report, o driveOptions, token string, order []card, total int, start time.Time) []appended {
	interval := time.Duration(float64(time.Second) / o.rate)
	var events []appended
	for index := range total {
		select {
		case <-ctx.Done():
			return events
		case <-time.After(time.Until(start.Add(time.Duration(index) * interval))):
		}
		target := order[index%len(order)]
		role := phaseRoles[target.Phase]
		if role == "" {
			role = "implementer"
		}
		type origin struct {
			SessionTitle string `json:"session_title"`
		}
		type actor struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			Origin origin `json:"origin"`
		}
		payload, err := json.Marshal(struct {
			Title string `json:"title"`
			Actor actor  `json:"actor"`
		}{
			Title: fmt.Sprintf("%s · bench edit %d", target.Title, index+1),
			Actor: actor{Kind: "session", ID: fmt.Sprintf("s-%s-%s-bench-%d", role, target.Key, index+1),
				Origin: origin{SessionTitle: "Legion " + role + " · " + target.Key}},
		})
		if err != nil {
			r.fail("event %d on %s: %v", index+1, target.Key, err)
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPatch, o.url+"/api/v1/issues/"+target.Key, bytes.NewReader(payload))
		if err != nil {
			r.fail("event %d on %s: %v", index+1, target.Key, err)
			continue
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		event := appended{Key: target.Key, Sent: time.Now()}
		response, err := http.DefaultClient.Do(request)
		event.Answered = time.Now()
		if err != nil {
			r.fail("event %d on %s: %v", index+1, target.Key, err)
			continue
		}
		answer, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		event.Status = response.StatusCode
		if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
			r.fail("event %d on %s answered %d: %s", index+1, target.Key, response.StatusCode, oneLine(answer))
			continue
		}
		events = append(events, event)
	}
	return events
}

// oneLine is body on one line, cut to 300 bytes.
func oneLine(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

// judgeEvents holds each event's first push to pushBound and answers the longest gap, in
// milliseconds.
func judgeEvents(r *report, events []appended, frames []frame) float64 {
	var gaps []float64
	over := 0
	for _, event := range events {
		index := slices.IndexFunc(frames, func(f frame) bool { return f.pushed() && f.At.After(event.Sent) })
		if index < 0 {
			r.fail("the event on %s sent at %s has no push after it", event.Key, event.Sent.UTC().Format(time.RFC3339Nano))
			over++
			continue
		}
		gap := frames[index].At.Sub(event.Sent)
		gaps = append(gaps, float64(gap.Microseconds())/1000)
		if gap > pushBound {
			r.fail("the event on %s sent at %s reached the view %s later, above %s", event.Key,
				event.Sent.UTC().Format(time.RFC3339Nano), gap.Round(time.Millisecond), pushBound)
			over++
		}
	}
	longest := slices.Max(append(gaps, 0))
	r.result("events count=%d first_push_median_ms=%.1f first_push_p90_ms=%.1f longest_gap_ms=%.1f over_bound=%d",
		len(events), median(gaps), percentile(gaps, 0.9), longest, over)
	return longest
}

// factsChange is one move of a stored row the drive watched: its issue and its new updated_at.
type factsChange struct {
	Key     string    `json:"key"`
	Updated time.Time `json:"updated_at"`
}

// watchFacts reads, every 100 ms until ctx ends, the updated_at of the project's stored rows of
// keys, and answers each move after since.
func watchFacts(ctx context.Context, pool *store.Pool, project string, keys []string, since time.Time) []factsChange {
	last := map[string]time.Time{}
	var changes []factsChange
	read := func(record bool) {
		rows, err := pool.Query(ctx, `select issue_key, updated_at from issue_facts where project_key = $1 and issue_key = any($2)`, project, keys)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var updated time.Time
			if rows.Scan(&key, &updated) != nil {
				return
			}
			if previous, seen := last[key]; record && (!seen || !previous.Equal(updated)) && updated.After(since) {
				changes = append(changes, factsChange{Key: key, Updated: updated})
			}
			last[key] = updated
		}
	}
	read(false)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// One last read, on a context of its own, so a move just before the window closed counts.
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx = final
			read(true)
			return changes
		case <-ticker.C:
			read(true)
		}
	}
}

// judgeFactsChanges holds the push that carries each change's entry to pushBound from the row's
// updated_at, and answers the longest gap, in milliseconds.
func judgeFactsChanges(r *report, changes []factsChange, frames []frame) float64 {
	var gaps []float64
	over := 0
	for _, change := range changes {
		index := slices.IndexFunc(frames, func(f frame) bool {
			return f.Type == "delta" && !f.At.Before(change.Updated) && slices.Contains(f.issues, change.Key)
		})
		if index < 0 {
			r.fail("the stored row of %s written at %s reached the view in no push", change.Key, change.Updated.UTC().Format(time.RFC3339Nano))
			over++
			continue
		}
		gap := frames[index].At.Sub(change.Updated)
		gaps = append(gaps, float64(gap.Microseconds())/1000)
		if gap > pushBound {
			r.fail("the stored row of %s written at %s reached the view %s later, above %s", change.Key,
				change.Updated.UTC().Format(time.RFC3339Nano), gap.Round(time.Millisecond), pushBound)
			over++
		}
	}
	longest := slices.Max(append(gaps, 0))
	r.result("facts_changes count=%d push_median_ms=%.1f push_p90_ms=%.1f longest_gap_ms=%.1f over_bound=%d",
		len(changes), median(gaps), percentile(gaps, 0.9), longest, over)
	if len(changes) == 0 {
		r.fail("the events caused no issue_facts change the drive saw, so no push of an entry was judged")
	}
	return longest
}

// recomputeLine matches the live hub's line for one recompute of a legion view up to its
// elapsed_ms, which is all the drive reads of it. The line goes on, after its counts, with the
// time of each stage of the recompute (read_ms, compute_ms, encode_ms, revision_ms and diff_ms),
// the parts of its read (read_cards_ms, read_facts_ms, read_grow_ms, read_cursor_ms, read_wait_ms
// and read_other_ms) and what marked the view (marks), the hub's finishRecompute, which a run's
// server log keeps for whoever asks where a slow recompute's time went and what woke it; the
// drive judges the whole recompute and leaves them.
var recomputeLine = regexp.MustCompile(`dispatch: delivery live recompute view=legion cause=(\S+) elapsed_ms=(\d+)`)

// readRecomputes reads the legion view's recompute lines the server logged between from and to,
// two offsets of its log; a line cut at to is left out.
func readRecomputes(path string, from, to int64) ([]recompute, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.NewSectionReader(file, from, to-from))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	var lines []recompute
	for scanner.Scan() {
		match := recomputeLine.FindSubmatch(scanner.Bytes())
		if match == nil {
			continue
		}
		elapsed, err := strconv.ParseFloat(string(match[2]), 64)
		if err != nil {
			continue
		}
		lines = append(lines, recompute{Cause: string(match[1]), ElapsedMs: elapsed})
	}
	return lines, scanner.Err()
}

// recomputeStats is lines by cause, and the median and the p90 of all of them.
func recomputeStats(lines []recompute) (map[string]stats, float64, float64) {
	byCause := map[string][]float64{}
	var all []float64
	for _, line := range lines {
		byCause[line.Cause] = append(byCause[line.Cause], line.ElapsedMs)
		all = append(all, line.ElapsedMs)
	}
	counted := map[string]stats{}
	for cause, values := range byCause {
		counted[cause] = stats{Count: len(values), MedianMs: median(values), P90Ms: percentile(values, 0.9)}
	}
	return counted, median(all), percentile(all, 0.9)
}

// gzipped is how many bytes body takes under gzip at its default level.
func gzipped(body []byte) int {
	var packed bytes.Buffer
	writer := gzip.NewWriter(&packed)
	_, _ = writer.Write(body)
	_ = writer.Close()
	return packed.Len()
}

// fileSize is how long path is now.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

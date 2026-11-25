// Package backpressure reads the model gateway's hold on new work: a JSON list the operator's
// `backpressure_url` serves, each object saying whether a gateway team, or "*" for every team, is
// held. While a hold stands for the daemon's team, the daemon reserves no paced wake for a moment
// before the hold ends and admits no issue (internal/supervise, internal/workflow, internal/admit).
//
// The reading fails open. A poll that fails, answers a non-success status or answers a body this
// package cannot read adds no hold and extends none; a hold ends when its lease passes, or at once
// when a newer answer says `open`. A lease is never read as reaching more than MaxLease past the
// moment the daemon received it, so a gateway that stops answering holds nothing for longer than
// that, and the daemon never needs a restart or an operator to be released.
package backpressure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

const (
	// MaxLease is the furthest past its receipt the daemon reads a hold's lease as reaching.
	MaxLease = 2 * time.Minute
	// PollEvery and PollSpread make the poll interval: PollEvery plus a draw from [0, PollSpread),
	// 10 to 15 seconds.
	PollEvery  = 10 * time.Second
	PollSpread = 5 * time.Second
	// RequestTimeout bounds one poll, below the shortest interval so polls never overlap.
	RequestTimeout = 5 * time.Second
	// maxBody is the largest answer read; a longer one is unreadable.
	maxBody = 1 << 20
	// AllTeams is the team of an object that holds every team.
	AllTeams = "*"
)

// Log lines, exported for the tests that read them.
const (
	BeganMessage   = "backpressure: the model gateway holds new work; no paced wake and no admission goes out until the hold ends"
	EndedMessage   = "backpressure: the model gateway's hold has ended; held wakes go out at worker_wakes_per_minute"
	FailingMessage = "backpressure: the model gateway's hold route did not answer; the daemon reads no hold from it until it answers again"
	AnswersMessage = "backpressure: the model gateway's hold route answers again"
)

// EndedBy says what ended a hold, in its end line.
const (
	EndedByGateway = "gateway"
	EndedByLease   = "lease"
)

// Hold is the daemon's reading of the gateway's hold for one team. It is safe for use from several
// goroutines. Every method takes the caller's now, so a test decides the whole reading with no
// clock.
type Hold struct {
	team string
	log  *slog.Logger

	mu sync.Mutex
	// kept is the newest object read for the team and for AllTeams, by that key.
	kept map[string]reading
	// standing is the hold the daemon has logged as begun and not yet as ended.
	standing *standing
	// failures counts the polls of the current run of failures; 0 while the route answers.
	failures int
}

// reading is one object of an answer, as kept.
type reading struct {
	team   string
	hold   bool
	until  time.Time // hold_until, cut to MaxLease past received
	at     time.Time
	reason string
	// received is when the daemon read the object. A reading older than MaxLease is forgotten, so
	// nothing the gateway said more than MaxLease ago decides anything.
	received time.Time
}

type standing struct {
	since      time.Time
	wakes      map[string]struct{}
	admissions map[string]struct{}
}

// View is the hold that stands, as the daemon's state shows it.
type View struct {
	// Team is the team the holding object names: the daemon's own team, or AllTeams.
	Team   string
	Reason string
	// Until is when the hold ends unless the gateway renews it, never more than MaxLease past the
	// answer that set it.
	Until time.Time
	// Since is when the daemon read the hold as begun.
	Since time.Time
	// Wakes and Admissions count the distinct paced wakes and issues the hold has put back since it
	// began.
	Wakes      int
	Admissions int
}

// New is the reading of team's hold, empty: nothing holds until an answer says so.
func New(team string, log *slog.Logger) *Hold {
	if log == nil {
		log = slog.Default()
	}
	return &Hold{team: team, log: log, kept: map[string]reading{}}
}

// Until is the end of the hold that stands at now, and the zero time when none stands.
func (h *Hold) Until(now time.Time) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.current(now)
	if !ok {
		return time.Time{}
	}
	return r.until
}

// HoldWake is the end of the hold that stands at now and true, having counted c's wake as one the
// hold puts back; the zero time and false while none stands.
func (h *Hold) HoldWake(c claim.Token, now time.Time) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.current(now)
	if !ok {
		return time.Time{}, false
	}
	h.standing.wakes[string(c)] = struct{}{}
	return r.until, true
}

// HoldAdmission is whether a hold stands at now, having counted issue as one it keeps from
// admission when one does.
func (h *Hold) HoldAdmission(issue string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.current(now); !ok {
		return false
	}
	h.standing.admissions[issue] = struct{}{}
	return true
}

// View is the hold that stands at now, and false when none stands.
func (h *Hold) View(now time.Time) (View, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.current(now)
	if !ok {
		return View{}, false
	}
	return View{
		Team: r.team, Reason: r.reason, Until: r.until, Since: h.standing.since,
		Wakes: len(h.standing.wakes), Admissions: len(h.standing.admissions),
	}, true
}

// current is the hold that stands at now: a kept reading that holds at now, while the hold is
// logged as begun and not as ended. A reader whose now is older than the poller's last settle
// finds no hold once the end is logged, so h.standing is never nil for a caller that finds one.
func (h *Hold) current(now time.Time) (reading, bool) {
	if h.standing == nil {
		return reading{}, false
	}
	return h.holding(now)
}

// holding is the kept reading that holds at now with the latest end.
func (h *Hold) holding(now time.Time) (reading, bool) {
	var best reading
	found := false
	for _, r := range h.kept {
		if !r.hold || !r.until.After(now) {
			continue
		}
		if !found || r.until.After(best.until) {
			best, found = r, true
		}
	}
	return best, found
}

// Settle ends a standing hold whose lease has passed at now, writing its end line. The poller
// calls it at each lease's end, so the line is written when the hold ends rather than at the next
// poll.
func (h *Hold) Settle(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settle(now)
}

func (h *Hold) settle(now time.Time) {
	if h.standing == nil {
		return
	}
	if _, ok := h.holding(now); !ok {
		h.end(now, EndedByLease)
	}
}

// next is the moment the standing hold's lease passes, the zero time while none stands.
func (h *Hold) next(now time.Time) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.current(now)
	if !ok {
		return time.Time{}
	}
	return r.until
}

func (h *Hold) end(now time.Time, by string) {
	h.log.Info(EndedMessage, "team", h.team, "endedBy", by, "heldFor", now.Sub(h.standing.since).Round(time.Second).String(),
		"heldWakes", len(h.standing.wakes), "heldAdmissions", len(h.standing.admissions))
	h.standing = nil
}

// Apply takes a readable answer the daemon received at received: for the team and for AllTeams,
// the newest object of the answer replaces the one kept when it is newer by `at`, or when the kept
// one is older than MaxLease. A hold whose lease passed before the answer is ended by its lease
// first; one that stood until the answer and stands no more after it is ended by the gateway.
func (h *Hold) Apply(answer []Object, received time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answered(received)
	h.settle(received)
	for _, key := range []string{h.team, AllTeams} {
		newest, ok := newestFor(answer, key)
		if !ok {
			continue
		}
		if kept, held := h.kept[key]; held && received.Sub(kept.received) < MaxLease && !newest.At.After(kept.at) {
			continue
		}
		until := newest.HoldUntil
		if limit := received.Add(MaxLease); until.After(limit) {
			until = limit
		}
		h.kept[key] = reading{team: key, hold: newest.State == StateHold, until: until, at: newest.At, reason: newest.Reason, received: received}
	}
	r, holds := h.holding(received)
	switch {
	case h.standing != nil && !holds:
		h.end(received, EndedByGateway)
	case h.standing == nil && holds:
		h.standing = &standing{since: received, wakes: map[string]struct{}{}, admissions: map[string]struct{}{}}
		h.log.Info(BeganMessage, "team", r.team, "reason", r.reason, "holdUntil", r.until.UTC().Format(time.RFC3339), "at", r.at.UTC().Format(time.RFC3339))
	}
}

// Fail takes a poll that read no answer at now. It adds no hold and extends none, and it is logged
// once for each run of failures.
func (h *Hold) Fail(err error, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures++
	if h.failures == 1 {
		h.log.Warn(FailingMessage, "team", h.team, "error", err.Error())
	}
	h.settle(now)
}

// answered ends a run of failures, with one line.
func (h *Hold) answered(now time.Time) {
	if h.failures == 0 {
		return
	}
	h.log.Info(AnswersMessage, "team", h.team, "failedPolls", h.failures)
	h.failures = 0
}

// The two values of an object's `state`.
const (
	StateHold = "hold"
	StateOpen = "open"
)

// Object is one object of the gateway's answer, read.
type Object struct {
	Team      string
	State     string
	HoldUntil time.Time
	At        time.Time
	Reason    string
}

// newestFor is the object of answer for team with the latest `at`.
func newestFor(answer []Object, team string) (Object, bool) {
	var newest Object
	found := false
	for _, o := range answer {
		if o.Team == team && (!found || o.At.After(newest.At)) {
			newest, found = o, true
		}
	}
	return newest, found
}

// wireObject is an object as the route writes it.
type wireObject struct {
	Team      string `json:"team"`
	State     string `json:"state"`
	HoldUntil string `json:"hold_until"`
	At        string `json:"at"`
	Reason    string `json:"reason"`
}

// Parse reads an answer: a JSON list of objects. Only the objects for team and AllTeams are read
// further, so an object of another team that this daemon cannot read costs it nothing; an object of
// its own that it cannot read makes the whole answer unreadable, which is no hold.
func Parse(body []byte, team string) ([]Object, error) {
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		return nil, errors.New("the answer is not a JSON list")
	}
	var wire []wireObject
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("read the answer: %w", err)
	}
	var objects []Object
	for i, w := range wire {
		if w.Team != team && w.Team != AllTeams {
			continue
		}
		o := Object{Team: w.Team, State: w.State, Reason: w.Reason}
		if w.State != StateHold && w.State != StateOpen {
			return nil, fmt.Errorf("object %d of team %q has state %q, not %q or %q", i, w.Team, w.State, StateHold, StateOpen)
		}
		at, err := time.Parse(time.RFC3339, w.At)
		if err != nil {
			return nil, fmt.Errorf("object %d of team %q: at: %w", i, w.Team, err)
		}
		o.At = at
		if w.State == StateHold {
			if o.HoldUntil, err = time.Parse(time.RFC3339, w.HoldUntil); err != nil {
				return nil, fmt.Errorf("object %d of team %q: hold_until: %w", i, w.Team, err)
			}
		}
		objects = append(objects, o)
	}
	return objects, nil
}

// Poller polls the route and keeps a Hold from what it answers.
type Poller struct {
	// URL is `backpressure_url`.
	URL string
	// TokenFile is `backpressure_token_file`, read at each poll so a rotated credential is taken
	// without a restart. Its content is sent as a bearer.
	TokenFile string
	Hold      *Hold
	// Client is nil for a client of its own.
	Client *http.Client
	// Now is the daemon's clock, which every reading of the hold is taken against; nil is the
	// wall clock. The waits between polls are the wall clock's.
	Now func() time.Time
	// Interval draws the wait before the next poll; nil is PollEvery plus a draw from
	// [0, PollSpread).
	Interval func() time.Duration
	// Timeout bounds one poll; zero is RequestTimeout.
	Timeout time.Duration
}

// Run polls at once, then after each interval, until ctx ends. Between two polls it wakes at the
// standing hold's lease end, so a hold that lapses is logged as ended when it ends.
func (p *Poller) Run(ctx context.Context) {
	for {
		p.Poll(ctx)
		due := time.Now().Add(p.interval())
		for {
			wait := time.Until(due)
			if wait <= 0 {
				break
			}
			if lapse := p.Hold.next(p.now()); !lapse.IsZero() {
				wait = min(wait, lapse.Sub(p.now()))
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			p.Hold.Settle(p.now())
		}
	}
}

// Poll makes one request and takes its answer, or its failure, into the hold.
func (p *Poller) Poll(ctx context.Context) {
	objects, err := p.fetch(ctx)
	switch {
	case ctx.Err() != nil:
		// The daemon is stopping: the request it cut short is no failure of the route's.
	case err != nil:
		p.Hold.Fail(err, p.now())
	default:
		p.Hold.Apply(objects, p.now())
	}
}

func (p *Poller) fetch(ctx context.Context) ([]Object, error) {
	token, err := os.ReadFile(p.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read backpressure_token_file: %w", err)
	}
	bearer := strings.TrimSpace(string(token))
	if bearer == "" {
		return nil, errors.New("backpressure_token_file is empty")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = RequestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read the answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("answered %s", resp.Status)
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("the answer is longer than %d bytes", maxBody)
	}
	return Parse(body, p.Hold.team)
}

func (p *Poller) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

func (p *Poller) interval() time.Duration {
	if p.Interval != nil {
		return p.Interval()
	}
	return PollEvery + rand.N(PollSpread)
}

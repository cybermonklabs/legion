// Package pause is a project's pause (dispatch://INFRA-3759): the daemon's flag that says whether
// its project is paused, and the service that pauses and resumes it. The pause's records and its
// two transactions are the store's (internal/store/pause.go). This package refuses a pause the
// daemon cannot hold, builds the rows those transactions queue, keeps the flag in step with the
// store, logs each act, and hands what runs after each commit to the daemon (Effects).
package pause

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The codes the API answers a refused pause or resume with (decision 20 of the plan).
const (
	// CodeReasonRequired is a pause whose reason is empty or white space alone
	// (ErrReasonRequired), answered 400.
	CodeReasonRequired = "PAUSE_REASON_REQUIRED"
	// CodeUnavailable is a pause this daemon's configuration cannot hold (*UnavailableError, which
	// names the setting), answered 409.
	CodeUnavailable = "PAUSE_UNAVAILABLE"
	// CodeAlreadyPaused is a pause while another stands (*store.AlreadyPausedError, which names the
	// pause that stands), answered 409.
	CodeAlreadyPaused = "PROJECT_ALREADY_PAUSED"
	// CodeNotPaused is a resume of a project that is not paused (*store.NotPausedError), answered
	// 409.
	CodeNotPaused = "PROJECT_NOT_PAUSED"
)

// ErrReasonRequired is a pause refused because its reason is empty or white space alone. The
// store refuses such a pause too (store.Store.Pause); this refusal comes first and writes nothing.
var ErrReasonRequired = errors.New("a pause needs a reason")

// HeldRowReleasedMessage is the line the resume writes for each row the pause held that it
// released (decision 23, Service.Resume). The daemon's stop transaction writes it too for the
// start it owes once its pause has ended with no pause open after it, so the log names that row
// as well.
const HeldRowReleasedMessage = "pause: a held row released"

// Setting is a setting of legion.yaml under which a daemon cannot hold a pause, spelled as the
// file spells it, with the path of its key as the config package's own errors name a key
// (config's readIdleRelease).
type Setting string

// The three settings under which a daemon cannot hold a pause (decision 20).
const (
	// SettingTmux is the tmux runtime, which has no idle release.
	SettingTmux Setting = "runtime: tmux"
	// SettingIdleReleaseOff is the idle release turned off (config.IdleReleaseConfig.Releases).
	SettingIdleReleaseOff Setting = "runtime.kubernetes.idle_release.idle_minutes: 0"
	// SettingWakePacingOff is the wake pacing turned off (config.Config.WorkerWakesPerMinute).
	SettingWakePacingOff Setting = "worker_wakes_per_minute: 0"
)

// Unavailable is the setting under which cfg's daemon cannot hold a pause, and false when it can.
// A pause stops a role as the idle release does and brings it back through the released role's
// wake path and the release's backstop, which run only under runtime: kubernetes with
// idle_minutes above 0, and the resume lets the held work out through the wake pacing, which
// worker_wakes_per_minute: 0 turns off (dispatch://INFRA-3759/artifact/decisions-md). `legion
// config check` names the same setting (decision 21).
func Unavailable(cfg config.Config) (Setting, bool) {
	switch {
	case cfg.Runtime.Kubernetes == nil:
		return SettingTmux, true
	case !cfg.Runtime.Kubernetes.IdleRelease.Releases():
		return SettingIdleReleaseOff, true
	case cfg.WorkerWakesPerMinute == 0:
		return SettingWakePacingOff, true
	}
	return "", false
}

// UnavailableError is a pause refused because the daemon runs under Setting (Unavailable).
type UnavailableError struct {
	Setting Setting
}

func (e *UnavailableError) Error() string {
	why := "the resume lets the held work out through the wake pacing, which this setting turns off"
	switch e.Setting {
	case SettingTmux:
		why = "a pause stops and wakes roles through the idle release, which runs only under runtime: kubernetes"
	case SettingIdleReleaseOff:
		why = "a pause stops and wakes roles through the idle release, which this setting turns off"
	}
	return fmt.Sprintf("this daemon cannot pause its project under %s: %s", e.Setting, why)
}

// Effects is what the daemon does once a pause or a resume has committed: the machines, the pod
// holds, the outbox lanes and the admission pass are the daemon's, so it supplies these steps, and
// a test runs the service with a fake of them. Each runs after the flag is set or cleared and
// before the act is logged, under the service's lock, so the next act, Load and Refresh wait for
// it: it must call nothing of the Service but Paused. Its context is the act's, never cancelled,
// because the transaction it follows has committed.
type Effects interface {
	// Paused runs once pause has committed and the flag is set. asked are the claims the pause's
	// transaction asked to stop; the daemon asks the stop of every other live role of the project
	// but the controller, which catches a launch that passed the gate before the flag was set
	// (decision 3).
	Paused(ctx context.Context, pause store.Pause, asked []store.PauseStopClaim)
	// Resumed runs once the resume that ended pause has committed and the flag is cleared: the
	// steps of decision 22 after the transaction, in their order, are the daemon's.
	Resumed(ctx context.Context, ended store.Pause)
}

// Options is what a Service is built with.
type Options struct {
	Store *store.Store
	// Records marks the rows the pause holds with the pause at its resume, and reads them for the
	// resume's log (record.Postgres.MarkPauseHeldRows), and reads whether a controller is
	// registered (record.Postgres.ControllerRegistered).
	Records *record.Postgres
	// Project is the project's Dispatch key ("LEGION"), the spelling of project_pauses and of the
	// outbox (internal/store/pause.go). The project token is derived from it where one is read.
	Project string
	// Config is the daemon's settled configuration, which says whether it can hold a pause
	// (Unavailable).
	Config config.Config
	// Effects runs after each commit; nil runs nothing.
	Effects Effects
	// Log takes the lines of decision 23 this package writes: the pause, the resume, and each held
	// row the resume released. nil is slog.Default().
	Log *slog.Logger
	// Now is the daemon's clock; nil is time.Now.
	Now func() time.Time
}

// Service pauses and resumes the daemon's project and holds its flag.
type Service struct {
	store       *store.Store
	records     *record.Postgres
	project     string
	unavailable Setting
	effects     Effects
	log         *slog.Logger
	now         func() time.Time

	// mu serializes Pause, Resume, Load and Refresh, each from before its store read or write to
	// after its set of the flag, so a Refresh never writes a reading taken before this daemon's
	// own act over the flag that act set.
	mu sync.Mutex
	// paused is the open pause's id, 0 while the project runs. A pause's id is never 0 (bigserial).
	paused atomic.Int64
}

// New builds a Service. The flag reads no pause until Load.
func New(opts Options) (*Service, error) {
	if opts.Store == nil || opts.Records == nil {
		return nil, errors.New("pause service: needs the store and the outbox reader")
	}
	if _, err := claim.ProjectToken(opts.Project); err != nil {
		return nil, fmt.Errorf("pause service: %w", err)
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	unavailable, _ := Unavailable(opts.Config)
	return &Service{store: opts.Store, records: opts.Records, project: opts.Project, unavailable: unavailable,
		effects: opts.Effects, log: opts.Log, now: opts.Now}, nil
}

// Paused is the open pause's id and true while the project is paused, as this daemon last read
// or made it (decision 18). It is one atomic load, cheap enough for every launch, task send and
// stop write to call, and the method value fits a func() (int64, bool).
func (s *Service) Paused() (int64, bool) {
	id := s.paused.Load()
	return id, id != 0
}

// Pause pauses the project for reason, by `by` (decisions 3, 20 and 24). It refuses a blank reason
// (ErrReasonRequired) and a daemon that cannot hold a pause (*UnavailableError), then runs the
// store's pause transaction, which refuses a second pause with the *store.AlreadyPausedError it
// returns unchanged. Once that commits it sets the flag, runs Effects.Paused with the claims the
// transaction asked to stop, and logs the pause. A caller that goes away does not stop an act
// under way, so no pause commits without its flag and its after-commit steps.
func (s *Service) Pause(ctx context.Context, by store.PauseActor, reason string) (store.Pause, error) {
	if strings.TrimSpace(reason) == "" {
		return store.Pause{}, ErrReasonRequired
	}
	if s.unavailable != "" {
		return store.Pause{}, &UnavailableError{Setting: s.unavailable}
	}
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	tell, err := s.tellsController(ctx, by)
	if err != nil {
		return store.Pause{}, err
	}
	req := store.PauseRequest{Project: s.project, By: by, Reason: reason, At: s.now(), StopRow: StopRow}
	if tell {
		req.Notice = pausedNotice
	}
	pause, asked, err := s.store.Pause(ctx, req)
	if standing := (*store.AlreadyPausedError)(nil); errors.As(err, &standing) {
		// The refusal read the pause that stands, which another daemon of a rolling restart may
		// have made since this one's last Refresh: the flag holds it from now.
		s.paused.Store(standing.Standing.ID)
	}
	if err != nil {
		return store.Pause{}, err
	}
	s.paused.Store(pause.ID)
	if s.effects != nil {
		s.effects.Paused(ctx, pause, asked)
	}
	claims := make([]string, len(asked))
	for i, stop := range asked {
		claims[i] = string(stop.Claim)
	}
	s.log.Info("pause: the project is paused", "pause", pause.ID, "by", string(pause.By.Kind), "by_session", pause.By.Session,
		"reason", pause.Reason, "stops", len(asked), "claims", claims, "told_controller", tell)
	return pause, nil
}

// Resume ends the project's pause, by `by` (decisions 22 and 24). The store's resume transaction
// marks each row the pause holds with the pause, and reads those rows, before it ends the pause
// (record.Postgres.MarkPauseHeldRows), so a row that runs after the resume and wakes a released
// role takes that pause's held wake; it refuses a project that is not paused with the
// *store.NotPausedError it returns unchanged. Once it commits, Resume clears the flag, runs
// Effects.Resumed, and logs the resume and one line for each held row it released. It answers
// the ended pause and those rows. Resume is never refused for the daemon's configuration: a pause
// another daemon made is always this one's to end.
func (s *Service) Resume(ctx context.Context, by store.PauseActor) (store.Pause, []record.PauseHeldRow, error) {
	ctx = context.WithoutCancel(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	tell, err := s.tellsController(ctx, by)
	if err != nil {
		return store.Pause{}, nil, err
	}
	at := s.now()
	var held []record.PauseHeldRow
	req := store.ResumeRequest{Project: s.project, By: by, At: at, MarkHeld: func(ctx context.Context, tx pgx.Tx, pause store.Pause) error {
		// The rows are read at a moment taken once the transaction holds the pause's row, not at
		// the resume's moment, which precedes the wait for that lock: a row that falls due during
		// the wait is held by the pause, and the earlier moment would leave it unmarked.
		var err error
		held, err = s.records.MarkPauseHeldRows(ctx, tx, s.project, pause.ID, s.now())
		return err
	}}
	if tell {
		req.Notice = resumedNotice
	}
	resumed, err := s.store.Resume(ctx, req)
	if notPaused := (*store.NotPausedError)(nil); errors.As(err, &notPaused) {
		// The refusal read no open pause, which another daemon may have ended since this one's
		// last Refresh: the flag reads none from now.
		s.paused.Store(0)
	}
	if err != nil {
		return store.Pause{}, nil, err
	}
	ended := resumed.Pause
	s.paused.Store(0)
	if s.effects != nil {
		s.effects.Resumed(ctx, ended)
	}
	s.log.Info("pause: the project is resumed", "pause", ended.ID, "by", string(ended.EndedBy.Kind), "by_session", ended.EndedBy.Session,
		"paused_by", string(ended.By.Kind), "paused_by_session", ended.By.Session, "held", len(held), "told_controller", tell)
	for _, row := range held {
		s.log.Info(HeldRowReleasedMessage, "pause", ended.ID, "row", row.ID, "kind", string(row.Kind), "issue", row.Issue,
			"claim", string(row.Claim))
	}
	return ended, held, nil
}

// Load reads the project's open pause and sets the flag from it: the boot calls it before
// supervision starts, so no launch runs before the flag holds a pause another daemon or an
// earlier boot made (decision 18). It answers that pause, and whether one is open. A failed read
// leaves the flag as it was.
func (s *Service) Load(ctx context.Context) (store.Pause, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	open, found, err := s.store.OpenPause(ctx, s.project)
	if err != nil {
		return store.Pause{}, false, err
	}
	s.paused.Store(open.ID)
	return open, found, nil
}

// Refresh is Load at each outbox sweep, so a pause made or ended through another daemon of a
// rolling restart reaches this daemon's flag within one sweep (decision 18).
func (s *Service) Refresh(ctx context.Context) error {
	_, _, err := s.Load(ctx)
	return err
}

// OpenPause is the project's open pause, and whether one is open, as the store holds it now.
func (s *Service) OpenPause(ctx context.Context) (store.Pause, bool, error) {
	return s.store.OpenPause(ctx, s.project)
}

// LastEndedPause is the project's newest ended pause, and whether one has ended (decision 25's
// `lastPause`).
func (s *Service) LastEndedPause(ctx context.Context) (store.Pause, bool, error) {
	return s.store.LastEndedPause(ctx, s.project)
}

// tellsController is whether an act by `by` queues the controller's notice (decision 24). The
// operator's act does and the controller's own does not, and only while a controller is
// registered, which is the rule every wake of the controller keeps (admit.Admission.wakeController):
// with none registered nothing reads the notice, since a controller that starts later is handed
// nothing published before it (notify.ControllerTopic) and reads the pause from the state instead
// (decision 25). The pause's notice has no rule of a waiting notice of its kind, as that function
// has: each names its own pause, who acted and why, which the next notice does not repeat. The
// registration is read just before the act's transaction, which reads it no more firmly: a
// registration takes neither the fact serializer nor the pause's row.
func (s *Service) tellsController(ctx context.Context, by store.PauseActor) (bool, error) {
	if by.Kind != store.PauseByOperator {
		return false, nil
	}
	var registered bool
	if err := s.store.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		registered, err = s.records.ControllerRegistered(ctx, tx, s.project)
		return err
	}); err != nil {
		return false, fmt.Errorf("pause %s: %w", s.project, err)
	}
	return registered, nil
}

// StopRow is the `pause_stop` row of one claim the pause asks to stop (decision 4): a supervise row
// naming the pause, the claim's role, tree and generation, and a reason the stopped claim's log
// line carries, keyed as StopKey keys it. The pause's transaction builds its rows with it, and so
// do the daemon's catch-up after a pause and its boot's pass while paused
// (store.Store.AskPauseStops, AskLivePauseStops), so every stop of a pause is one row of one
// shape. A row keyed by the project names the claim's issue in its payload
// (record.SuperviseRequest.Issue), which the executor reads its claim from.
func StopRow(pause store.Pause, stop store.PauseStopClaim) (store.PauseRow, error) {
	request := record.SuperviseRequest{Op: "pause_stop", Tree: stop.Tree, Role: stop.Role,
		Generation: stop.Generation, Pause: pause.ID,
		Reason: fmt.Sprintf("project %s is paused (pause %d): %s", pause.Project, pause.ID, pause.Reason)}
	key := StopKey(pause, stop.Issue)
	if key != stop.Issue {
		request.Issue = stop.Issue
	}
	return pauseRow(key, request, pause.Since)
}

// StopKey is the outbox key (record.OutboxRow.Issue) of pause's `pause_stop` row of a claim of
// issue, and so the key of the lifecycle lane that runs it: the issue itself where it is an issue
// of pause's project, and otherwise the project's own key, a project-level row. The outbox runs
// only the rows whose key names its own project (record's split_part(issue, '-', 1) scope), so a
// row keyed by another project's issue would never run, and the resume's delete of the pause's
// rows that have not run would never find it. Such a claim is one the operator's spawn made
// under this daemon's project token (api's spawn), which the pause stops as it stops every other.
func StopKey(pause store.Pause, issue string) string {
	if project, _, _ := strings.Cut(issue, "-"); project == pause.Project {
		return issue
	}
	return pause.Project
}

// pausedNotice is the controller's project-paused notice: who paused, when, and why.
func pausedNotice(pause store.Pause) (store.PauseRow, error) {
	return pauseRow(pause.Project, record.ControllerNotice{Kind: record.ProjectPausedNotice,
		Pause: &record.ProjectPause{ID: pause.ID, By: actor(pause.By), At: pause.Since, Reason: pause.Reason}}, pause.Since)
}

// resumedNotice is the controller's project-resumed notice: who resumed, when, and the pause it
// ended.
func resumedNotice(ended store.Pause) (store.PauseRow, error) {
	return pauseRow(ended.Project, record.ControllerNotice{Kind: record.ProjectResumedNotice,
		Pause: &record.ProjectPause{ID: ended.ID, By: actor(ended.EndedBy), At: ended.EndedAt}}, ended.EndedAt)
}

// pauseRow encodes payload through record's own constructor, which validates it, as the store's
// pause transaction takes a row (store.PauseRow), keyed by issue. A pause's notice is keyed by the
// project, of which the daemon records no run, so it names the fence of a key the record holds
// nothing of (record.FirstFence), which is the fence its delivery reads too (record.ReadFence): a
// controller notice names its run and entry as every notice does (record.Notice.Generation). A
// stop names no fence (record.FenceOf), and Fenced leaves it as it is.
func pauseRow(issue string, payload record.OutboxPayload, at time.Time) (store.PauseRow, error) {
	row, err := record.NewOutboxRow(issue, record.Fenced(payload, record.FirstFence), at)
	if err != nil {
		return store.PauseRow{}, err
	}
	return store.PauseRow{Issue: row.Issue, Kind: string(row.Kind), Payload: row.Payload}, nil
}

func actor(a store.PauseActor) record.PauseActor {
	return record.PauseActor{Kind: string(a.Kind), Session: a.Session}
}

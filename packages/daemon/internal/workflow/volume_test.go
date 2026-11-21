package workflow

// What an issue's architect is told when the volume its pod's roles share fills under them, and
// what holds that telling back: the bounds of volumeNearFull, each driven through the store the
// daemon counts them in, because a daemon that restarts mid-episode must not start the count over
// (INFRA-2009).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// volumeAt is when the first sweep of these tests reports, and volumePod the uid of the pod whose
// launcher reported it.
var volumeAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

const volumePod = "pod-uid-1"

// volumeRig is one issue whose pod's cache trimmer reports: the pool, the engine on a clock the
// test moves, the journal its two lines are read from, and apply, which applies one report under
// an id of its own.
type volumeRig struct {
	pool   *pgxpool.Pool
	engine *Engine
	clock  *checkClock
	logged *bytes.Buffer
	apply  func(id string, fact intake.Fact) intake.Result
	// reports counts the reports the rig has applied, so each gets an event id of its own, as the
	// daemon's own wiring gives each report one (volumeTrimReports.report).
	reports int
}

func newVolumeRig(t *testing.T) *volumeRig {
	t.Helper()
	pool := migratedPool(t)
	logged := &bytes.Buffer{}
	clock := &checkClock{now: volumeAt}
	engine := New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(),
		Pods: wakePods(t, pool), Linger: time.Hour, Clock: clock.Now},
		slog.New(slog.NewTextHandler(logged, nil)))
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7,
		Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	return &volumeRig{pool: pool, engine: engine, clock: clock, logged: logged, apply: applyFacts(t, pool, engine)}
}

// report applies one sweep of the issue's pod, with whatever of the report the case needs.
func (r *volumeRig) report(t *testing.T, sweep record.VolumeNearFull) {
	t.Helper()
	r.reports++
	if sweep.Trimmer == "" {
		sweep.Trimmer = "trim-1"
	}
	if sweep.Episode == 0 {
		sweep.Episode = 1
	}
	if sweep.RaisedBy == "" {
		sweep.RaisedBy = shimwire.VolumeEpisodeFirst
	}
	r.apply(fmt.Sprintf("volume-%d", r.reports), intake.VolumeNearFull{Issue: "LEGION-208", PodUID: volumePod, Report: sweep})
}

// notices is every volume-near-full notice the outbox holds for the issue, in order.
func (r *volumeRig) notices(t *testing.T) []record.Notice {
	t.Helper()
	return noticesOfKind(t, r.pool, "LEGION-208", record.VolumeNearFullNotice)
}

// counted is the notices the store has counted for one trimmer and episode of the pod, read
// through a store handle of the caller's: a second handle is a restarted daemon's.
func counted(t *testing.T, pods Pods, pool *pgxpool.Pool, trimmer string, episode int) store.VolumeNoticeCounts {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the read of the counts: %v", err)
	}
	defer tx.Rollback(context.Background())
	counts, err := pods.VolumeNoticeCounts(context.Background(), tx, "LEGION-208", volumePod, trimmer, episode)
	if err != nil {
		t.Fatalf("read the counts of %s episode %d: %v", trimmer, episode, err)
	}
	return counts
}

// fullSweep is the report the first test asserts on whole: a volume still above its low mark
// after a sweep, with everything the trimmer measured.
func fullSweep() record.VolumeNearFull {
	return record.VolumeNearFull{
		Trimmer: "trim-1", Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
		UsedBytes: 48 << 30, FreeBytes: 2 << 30, FreedBytes: 3 << 30,
		StopReason:   "nothing left older than the minimum age",
		RefusedReads: []string{"pid 42 (/proc/42/fd)"},
		Sizes: []record.VolumeSize{
			{Path: "caches/uv", Bytes: 12 << 30, BytesOverHour: 11 << 30, BytesOverDay: 9 << 30},
			{Path: "workspaces/LEGION-208", Bytes: 20 << 30, BytesOverHour: 2 << 30, BytesOverDay: 0},
		},
		SizesAt: volumeAt.Add(-time.Minute),
	}
}

// One sweep that ended with the volume still above its low mark writes exactly one notice, to the
// issue's own architect, carrying everything the architect needs to act as fields of its record:
// which pod, how full, what the sweep freed, why it stopped, the /proc read the kernel refused, and
// what is on the volume with the two age splits that tell a working set from cold bytes. It writes
// no sentence of them: the architect's prompt says how to read the fields, the kind of size those
// are among them (texts_steps_test.go).
func TestASweepThatLeavesTheVolumeFullTellsTheArchitectOnce(t *testing.T) {
	rig := newVolumeRig(t)
	rig.report(t, fullSweep())

	notices := rig.notices(t)
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d volume-near-full notices, want exactly one", len(notices))
	}
	notice := notices[0]
	if entry, _ := texts.Lookup(notice.Kind); !slices.Equal(entry.Readers, []claim.Role{claim.RoleArchitect}) || notice.Issue != "LEGION-208" {
		t.Fatalf("the notice of %q is read by %v, want LEGION-208's architect alone", notice.Issue, entry.Readers)
	}
	if notice.Volume == nil {
		t.Fatalf("the notice carries no volume sweep: %+v", notice)
	}
	volume := *notice.Volume
	if volume.PodUID != volumePod || volume.Trimmer != "trim-1" || volume.Episode != 1 {
		t.Fatalf("the notice names pod %q trimmer %q episode %d, want %q, trim-1 and 1", volume.PodUID, volume.Trimmer, volume.Episode, volumePod)
	}
	if volume.UsedBytes != 48<<30 || volume.FreeBytes != 2<<30 || volume.FreedBytes != 3<<30 {
		t.Fatalf("the notice names used=%d free=%d freed=%d, want the sweep's own numbers", volume.UsedBytes, volume.FreeBytes, volume.FreedBytes)
	}
	if volume.StopReason != "nothing left older than the minimum age" {
		t.Fatalf("the notice says the sweep stopped because %q, want the trimmer's own words", volume.StopReason)
	}
	if len(volume.RefusedReads) != 1 || volume.RefusedReads[0] != "pid 42 (/proc/42/fd)" {
		t.Fatalf("the notice names refused reads %v, want the one the sweep met", volume.RefusedReads)
	}
	if len(volume.Sizes) != 2 || volume.Sizes[0] != (record.VolumeSize{Path: "caches/uv", Bytes: 12 << 30, BytesOverHour: 11 << 30, BytesOverDay: 9 << 30}) {
		t.Fatalf("the notice carries sizes %+v, want the walk's own entries with both age splits", volume.Sizes)
	}
	if notice.Reason != "" {
		t.Errorf("the notice writes a sentence of the sweep, %q, want its facts alone", notice.Reason)
	}
	if !strings.Contains(rig.logged.String(), VolumeNoticeWritten) {
		t.Errorf("the daemon wrote no %q line for the notice: %s", VolumeNoticeWritten, rig.logged.String())
	}
	for _, want := range []string{"LEGION-208", volumePod, "trim-1", "episode=1", "notices=1"} {
		if !strings.Contains(rig.logged.String(), want) {
			t.Errorf("the written line does not name %q: %s", want, rig.logged.String())
		}
	}
}

// A second sweep of the same pod inside the hour tells nobody: a filling pod reports every
// reading interval, and each notice is a wake of the architect's session.
func TestASecondSweepInsideTheHourTellsNobody(t *testing.T) {
	rig := newVolumeRig(t)
	rig.report(t, fullSweep())
	rig.clock.now = volumeAt.Add(59 * time.Minute)
	rig.report(t, fullSweep())

	if notices := rig.notices(t); len(notices) != 1 {
		t.Fatalf("the outbox holds %d notices after a second sweep inside the hour, want the first alone", len(notices))
	}
	if !strings.Contains(rig.logged.String(), VolumeNoticeSuppressed) {
		t.Errorf("the daemon wrote no %q line for the held report: %s", VolumeNoticeSuppressed, rig.logged.String())
	}
	for _, want := range []string{"LEGION-208", volumePod, "trim-1", "episode=1", "notices=1"} {
		if !strings.Contains(rig.logged.String(), want) {
			t.Errorf("the suppressed line does not name %q: %s", want, rig.logged.String())
		}
	}

	// Past the hour it is told again, which is what makes the line above a bound and not a wall.
	rig.clock.now = volumeAt.Add(61 * time.Minute)
	rig.report(t, fullSweep())
	if notices := rig.notices(t); len(notices) != 2 {
		t.Fatalf("the outbox holds %d notices an hour and a minute on, want a second", len(notices))
	}
}

// Three notices for one trimmer and episode is all an architect hears of that episode, however
// long it runs: a fourth says nothing the third did not.
func TestAFourthSweepOfOneTrimmerAndEpisodeTellsNobodyHoweverLongItWaits(t *testing.T) {
	rig := newVolumeRig(t)
	for at := range VolumeNoticeCap {
		rig.clock.now = volumeAt.Add(time.Duration(at) * 2 * time.Hour)
		rig.report(t, fullSweep())
	}
	if notices := rig.notices(t); len(notices) != VolumeNoticeCap {
		t.Fatalf("the outbox holds %d notices of three sweeps hours apart, want %d", len(notices), VolumeNoticeCap)
	}
	// A day later, well past every hour: the count is what holds it, not the clock.
	rig.clock.now = volumeAt.Add(24 * time.Hour)
	rig.report(t, fullSweep())
	if notices := rig.notices(t); len(notices) != VolumeNoticeCap {
		t.Fatalf("the outbox holds %d notices after a fourth sweep a day later, want the three the cap gives", len(notices))
	}
	if !strings.Contains(rig.logged.String(), VolumeNoticeSuppressed) {
		t.Errorf("the daemon wrote no %q line for the fourth report: %s", VolumeNoticeSuppressed, rig.logged.String())
	}
}

// A restarted roles container mints a new trimmer identifier, and that is a counting line of its
// own: the fourth report of the old trimmer is silent and the first of the new one is told, in
// the same hour.
func TestANewTrimmerStartsTheCountOver(t *testing.T) {
	rig := newVolumeRig(t)
	for at := range VolumeNoticeCap {
		rig.clock.now = volumeAt.Add(time.Duration(at) * 2 * time.Hour)
		rig.report(t, fullSweep())
	}
	rig.clock.now = volumeAt.Add(24 * time.Hour)
	rig.report(t, fullSweep())
	if notices := rig.notices(t); len(notices) != VolumeNoticeCap {
		t.Fatalf("the old trimmer's fourth report was told: %d notices, want %d", len(notices), VolumeNoticeCap)
	}

	fresh := fullSweep()
	fresh.Trimmer, fresh.Episode = "trim-2", 1
	rig.report(t, fresh)
	notices := rig.notices(t)
	if len(notices) != VolumeNoticeCap+1 {
		t.Fatalf("the new trimmer's first report was not told: %d notices, want %d", len(notices), VolumeNoticeCap+1)
	}
	if last := notices[len(notices)-1]; last.Volume == nil || last.Volume.Trimmer != "trim-2" {
		t.Fatalf("the last notice is %+v, want the new trimmer's own", last.Volume)
	}
}

// A report whose episode a rise raised is sent however recent the pod's last notice was. The
// launcher raises an episode that way at the start of a reported sweep, where the lower of that
// start and the one before it stands five points or more above the episode's anchor — the lowest
// start of its reported sweeps where no rise raised it, the floor that raised it where one did —
// and at no smaller climb (internal/launcher's riseAt), so no rise follows an anchor above 95%, a
// reading being at most 100%; what this case holds is what the daemon does with the word.
func TestARiseRaisedSweepIsToldAtOnce(t *testing.T) {
	rig := newVolumeRig(t)
	rig.report(t, fullSweep())
	rig.clock.now = volumeAt.Add(time.Minute)
	risen := fullSweep()
	risen.Episode, risen.RaisedBy, risen.UsedBytes = 2, shimwire.VolumeEpisodeRise, 49<<30
	rig.report(t, risen)

	notices := rig.notices(t)
	if len(notices) != 2 {
		t.Fatalf("the outbox holds %d notices a minute after the first, want the rise told at once", len(notices))
	}
	if last := notices[1]; last.Volume == nil || last.Volume.RaisedBy != shimwire.VolumeEpisodeRise {
		t.Fatalf("the second notice is %+v, want the rise-raised episode's", last.Volume)
	}

	// The cap still holds it: three of that episode and no more, whatever raised them.
	for at := 2; at <= 4; at++ {
		rig.clock.now = volumeAt.Add(time.Duration(at) * time.Minute)
		rig.report(t, risen)
	}
	if notices := rig.notices(t); len(notices) != 1+VolumeNoticeCap {
		t.Fatalf("the outbox holds %d notices, want the first plus the %d the rise-raised episode's cap gives", len(notices), VolumeNoticeCap)
	}
}

// A recovery — the volume fell under its low mark and filled again — waits for the hour: it is
// the volume doing what it did before, and the architect already heard it once.
func TestARecoveryRaisedSweepWaitsForTheHour(t *testing.T) {
	rig := newVolumeRig(t)
	rig.report(t, fullSweep())
	rig.clock.now = volumeAt.Add(time.Minute)
	recovered := fullSweep()
	recovered.Episode, recovered.RaisedBy = 2, shimwire.VolumeEpisodeRecovery
	rig.report(t, recovered)

	if notices := rig.notices(t); len(notices) != 1 {
		t.Fatalf("the outbox holds %d notices a minute after the first, want the recovery held for the hour", len(notices))
	}
	rig.clock.now = volumeAt.Add(61 * time.Minute)
	rig.report(t, recovered)
	notices := rig.notices(t)
	if len(notices) != 2 {
		t.Fatalf("the outbox holds %d notices past the hour, want the recovery told then", len(notices))
	}
	if last := notices[1]; last.Volume == nil || last.Volume.RaisedBy != shimwire.VolumeEpisodeRecovery {
		t.Fatalf("the second notice is %+v, want the recovery-raised episode's", last.Volume)
	}
}

// The trimmer switching itself off is the report no sweep makes, and the one the bounds must not
// swallow: the protection this notice exists for has stopped, on exactly the pod that has been
// reporting. It is told whatever the counts stand at, and it carries the faults with their stacks,
// each on one line with its line breaks written as Go writes them (VolumeNearFullNotice), and the
// last reading.
func TestASwitchedOffTrimmerIsToldWhateverTheCountsStandAt(t *testing.T) {
	rig := newVolumeRig(t)
	for at := range VolumeNoticeCap {
		rig.clock.now = volumeAt.Add(time.Duration(at) * 2 * time.Hour)
		rig.report(t, fullSweep())
	}
	rig.clock.now = volumeAt.Add(VolumeNoticeCap*2*time.Hour + time.Second)
	faults := []string{
		"read the volume: panic: runtime error: invalid memory address\ngoroutine 9 [running]:\nmain.sweep()\n\t/src/trim.go:11",
		"walk the sizes: panic: too many open files\ngoroutine 9 [running]:\nmain.walk()\n\t/src/trim.go:42",
		"delete a cache entry: panic: send on closed channel\ngoroutine 9 [running]:\nmain.unlink()\n\t/src/trim.go:77",
	}
	off := record.VolumeNearFull{Trimmer: "trim-1", Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
		UsedBytes: 49 << 30, FreeBytes: 1 << 30, StopReason: "the trimmer switched itself off after three faults",
		Faults: faults, SwitchedOff: true}
	rig.report(t, off)

	notices := rig.notices(t)
	if len(notices) != VolumeNoticeCap+1 {
		t.Fatalf("the outbox holds %d notices, want the %d of the episode plus the switched-off report", len(notices), VolumeNoticeCap)
	}
	last := notices[len(notices)-1]
	if last.Volume == nil || !last.Volume.SwitchedOff {
		t.Fatalf("the last notice is %+v, want the switched-off report", last.Volume)
	}
	if len(last.Volume.Faults) != 3 {
		t.Fatalf("the switched-off notice carries %d faults, want the three that stopped the trimmer", len(last.Volume.Faults))
	}
	for i, fault := range faults {
		if want := strings.ReplaceAll(fault, "\n", `\n`); last.Volume.Faults[i] != want {
			t.Errorf("fault %d is %q, want it whole with its stack on one line: %q", i, last.Volume.Faults[i], want)
		}
	}
	if last.Volume.UsedBytes != 49<<30 || last.Volume.FreeBytes != 1<<30 {
		t.Fatalf("the switched-off notice reads used=%d free=%d, want the last reading the trimmer took", last.Volume.UsedBytes, last.Volume.FreeBytes)
	}
	if len(last.Volume.Sizes) != 0 {
		t.Fatalf("the switched-off notice carries sizes %+v, want none: no sweep produced it", last.Volume.Sizes)
	}
}

// The count is spent where the notice row is written, in one transaction, so a notice the
// listener can hand to no architect — a role topic nobody holds — is still a notice spent. The
// engine writes the row and knows nothing of holders; what this drives is that the store's count
// moved with the row and not with any delivery.
func TestTheCountIsSpentWithTheRowWhateverBecomesOfIt(t *testing.T) {
	rig := newVolumeRig(t)
	pods := wakePods(t, rig.pool)
	if counts := counted(t, pods, rig.pool, "trim-1", 1); counts.Notices != 0 {
		t.Fatalf("the store counts %d notices before any report, want none", counts.Notices)
	}
	rig.report(t, fullSweep())
	counts := counted(t, pods, rig.pool, "trim-1", 1)
	if counts.Notices != 1 || !counts.LastAt.Equal(volumeAt) {
		t.Fatalf("the store counts %d notices last written at %s, want one at %s", counts.Notices, counts.LastAt, volumeAt)
	}
	if rows := outboxRows(t, rig.pool); rows != 1 {
		t.Fatalf("the outbox holds %d rows, want the one notice the count was spent on", rows)
	}
}

// The bounds hold across a restart of the daemon, because they are read from the store and never
// from this process: a daemon that restarted mid-episode and started the count over would tell
// the architect three more times, on exactly the pod that is reporting every minute. The restart
// is driven as the daemon restarts — a second store handle and a second engine over the same
// database — and the fourth report of the episode is still silent.
func TestTheBoundsHoldAcrossARestartOfTheDaemon(t *testing.T) {
	rig := newVolumeRig(t)
	for at := range VolumeNoticeCap {
		rig.clock.now = volumeAt.Add(time.Duration(at) * 2 * time.Hour)
		rig.report(t, fullSweep())
	}
	if notices := rig.notices(t); len(notices) != VolumeNoticeCap {
		t.Fatalf("the outbox holds %d notices before the restart, want %d", len(notices), VolumeNoticeCap)
	}

	// The restarted daemon: a store handle of its own over the same database, a fresh engine on
	// it, and nothing of the first process's memory.
	restarted := &volumeRig{pool: rig.pool, clock: &checkClock{now: volumeAt.Add(24 * time.Hour)}, logged: &bytes.Buffer{}}
	restarted.engine = New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(),
		Pods: wakePods(t, rig.pool), Linger: time.Hour, Clock: restarted.clock.Now},
		slog.New(slog.NewTextHandler(restarted.logged, nil)))
	restarted.apply = applyFacts(t, rig.pool, restarted.engine)
	restarted.reports = rig.reports + 100

	restarted.report(t, fullSweep())
	if notices := restarted.notices(t); len(notices) != VolumeNoticeCap {
		t.Fatalf("the restarted daemon wrote a %dth notice of the episode, want the store's count to hold it", len(notices))
	}
	if counts := counted(t, wakePods(t, rig.pool), rig.pool, "trim-1", 1); counts.Notices != VolumeNoticeCap {
		t.Fatalf("the store counts %d notices after the restart, want the %d written before it", counts.Notices, VolumeNoticeCap)
	}
	if !strings.Contains(restarted.logged.String(), VolumeNoticeSuppressed) {
		t.Errorf("the restarted daemon wrote no %q line: %s", VolumeNoticeSuppressed, restarted.logged.String())
	}
}

// An issue the daemon does not record, and one whose tree lingers after leaving the workflow,
// write nothing: there is nobody in such a tree to act on the notice.
func TestAVolumeReportOfAnIssueNobodyRunsWritesNothing(t *testing.T) {
	rig := newVolumeRig(t)
	rig.reports++
	rig.apply("volume-unknown", intake.VolumeNearFull{Issue: "LEGION-999", PodUID: volumePod, Report: fullSweep()})
	if rows := outboxRows(t, rig.pool); rows != 0 {
		t.Fatalf("the outbox holds %d rows for an issue the daemon does not record, want none", rows)
	}

	// The tree's own linger, as the close writes it: the root is done and holds a linger_until,
	// which is what record.TreeLingers reads.
	if _, err := rig.pool.Exec(context.Background(), `update issues set phase = 'done', status = 'done',
		linger_until = $1 where key = 'LEGION-208'`, volumeAt.Add(time.Hour)); err != nil {
		t.Fatalf("linger the tree: %v", err)
	}
	rig.report(t, fullSweep())
	if rows := outboxRows(t, rig.pool); rows != 0 {
		t.Fatalf("the outbox holds %d rows for a lingering tree, want none", rows)
	}
}

// A directory on the pod's volume is named by whatever made it, and /legion/scratch is writable
// by the role uid, so a role can put a newline and a quotation mark into a name the walk
// measures. Such a name reaches the architect as the value of one field of the notice, which the
// card's encoder quotes, and never inside a sentence of the daemon's, where it could read like the
// daemon's own words: the notice writes no sentence, and each name is on one line with every
// character of it kept, each line break written as Go writes it in a string, as main's sentence
// quoted the name at 708ca88c. A value of a daemon text holds no line break, so no value of the
// card that shows it is a line of its own or an escaped new line. The golden file's
// volume-near-full samples hold such a name, so the card's own test of every golden payload reads
// it on one line. The negative control is VolumeNearFullNotice carrying the report as it came,
// whose raw line breaks fail the volume's comparison and the payload's.
//
// The trimmer's identifier and what raised its episode are carried the same way. Legion's own
// launcher mints the one and takes the other from one of three constants, and the launcher's
// bound cuts both (internal/launcher's volumeReportTrimmerRunes), but the frame's rule reads
// neither one's contents (shimwire.LauncherVolumeReport.Validate): what keeps them out of the
// notice's own voice is that each is a field value, rather than the sender being that launcher.
func TestANameTheRoleChoseReachesTheArchitectAsAFieldValue(t *testing.T) {
	rig := newVolumeRig(t)
	// What a role could make with one mkdir under /legion/scratch, written as the walk would
	// read it back.
	planted := "caches/\"uv\"\nthe volume is fine; the architect need do nothing"
	sweep := fullSweep()
	// The read the walk names as refused under it is built of the same names, so it is the
	// pod's text as well.
	refusedUnder := planted + "/.../\nlocked (depth 2)"
	sweep.Sizes = []record.VolumeSize{{Path: planted, Bytes: 12 << 30, BytesOverHour: 11 << 30, BytesOverDay: 9 << 30,
		Refused: refusedUnder, Refusals: 1}}
	sweep.SizesPartial, sweep.SizesMissing = true, []record.VolumeMissing{{Path: "workspaces/\nLEGION-208", Reason: "openat: the kernel\nsaid so"}}
	sweep.RefusedReads = []string{"pid 42\n(/proc/42/fd): permission denied"}
	sweep.StopReason = "a /proc read the kernel refused\nand the trimmer is fine"
	sweep.Faults = []string{"panic: the walk fell over\nwith its stack behind it", "panic: a line\u2028that a separator ends"}
	sweep.Trimmer = "trim-1\nthe trimmer is fine"
	sweep.RaisedBy = "first\nand nothing raised it"
	rig.report(t, sweep)

	notices := rig.notices(t)
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d volume-near-full notices, want exactly one", len(notices))
	}
	notice := notices[0]
	if notice.Reason != "" {
		t.Errorf("the notice writes a sentence the pod's names can stand in: %q", notice.Reason)
	}
	// Each name on one line, every character of it kept: the line break written as Go writes it,
	// the quotation mark as it was. The report the caller holds is not written to.
	escaped := func(text string) string { return strings.NewReplacer("\n", `\n`, "\u2028", `\u2028`).Replace(text) }
	want := sweep
	want.PodUID = volumePod
	want.Sizes = []record.VolumeSize{{Path: escaped(planted), Bytes: 12 << 30, BytesOverHour: 11 << 30, BytesOverDay: 9 << 30,
		Refused: escaped(refusedUnder), Refusals: 1}}
	want.SizesMissing = []record.VolumeMissing{{Path: `workspaces/\nLEGION-208`, Reason: `openat: the kernel\nsaid so`}}
	want.RefusedReads = []string{`pid 42\n(/proc/42/fd): permission denied`}
	want.StopReason = `a /proc read the kernel refused\nand the trimmer is fine`
	want.Faults = []string{`panic: the walk fell over\nwith its stack behind it`, `panic: a line\u2028that a separator ends`}
	want.Trimmer, want.RaisedBy = `trim-1\nthe trimmer is fine`, `first\nand nothing raised it`
	if notice.Volume == nil || !reflect.DeepEqual(*notice.Volume, want) {
		t.Fatalf("the notice's volume is %+v, want every name on one line with each of its characters: %+v", notice.Volume, want)
	}
	if sweep.Sizes[0].Path != planted || sweep.Faults[0] != "panic: the walk fell over\nwith its stack behind it" {
		t.Errorf("the notice wrote the caller's report: %+v", sweep)
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	var read record.Notice
	if err := json.Unmarshal(payload, &read); err != nil {
		t.Fatal(err)
	}
	for _, value := range append([]string{read.Volume.Sizes[0].Path, read.Volume.Sizes[0].Refused, read.Volume.SizesMissing[0].Path,
		read.Volume.SizesMissing[0].Reason, read.Volume.StopReason, read.Volume.Trimmer, read.Volume.RaisedBy},
		append(read.Volume.RefusedReads, read.Volume.Faults...)...) {
		if strings.ContainsAny(value, "\n\r\u2028") {
			t.Errorf("a value of the notice's payload holds a line break, which the card shows as a line of its own or an escaped new line: %q", value)
		}
	}
}

// A size the walk could read only in part, a directory the walk could not open, and a walk its
// budget cut are three facts, and the notice keeps them apart as the report gave them: the size
// with its refused read and its count, the target the walk measured none of with the kernel's
// answer, and the cut walk's mark beside the count of targets the report left out. A reader tells
// a lower bound from a whole size by the size's own refused read, and a cut walk from a refused
// read by sizesPartial, as the architect's prompt says; for exactly these reports the sentence
// main once wrote read `that walk ended early and measured none of ` followed by nothing, with the
// unreadable directory's bytes given as the directory's own. Each case's volume crosses the outbox
// row, which a later daemon decodes strictly.
func TestTheNoticeTellsADirectoryReadInPartAndAWalkThatWasCutApart(t *testing.T) {
	const pack, under = "/legion/feed/pack", "/legion/feed/pack/under"
	for _, tc := range []struct {
		name  string
		sweep func(*record.VolumeNearFull)
	}{
		{
			name: "a read refused under a directory",
			sweep: func(sweep *record.VolumeNearFull) {
				sweep.Sizes = append(sweep.Sizes, record.VolumeSize{Path: pack, Bytes: 1, Refused: under, Refusals: 2})
			},
		},
		{
			name: "a directory the walk could not open",
			sweep: func(sweep *record.VolumeNearFull) {
				sweep.SizesMissing = []record.VolumeMissing{{Path: "/legion/scratch/sealed", Reason: "openat: permission denied"}}
			},
		},
		{
			name: "a walk its budget cut",
			sweep: func(sweep *record.VolumeNearFull) {
				sweep.SizesPartial, sweep.SizesMissing, sweep.SizesMissingOmitted = true, []record.VolumeMissing{{Path: "/legion/uv/cache"}}, 3
			},
		},
		{
			name:  "a cut walk that names nothing as unmeasured",
			sweep: func(sweep *record.VolumeNearFull) { sweep.SizesPartial = true },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVolumeRig(t)
			sweep := fullSweep()
			tc.sweep(&sweep)
			rig.report(t, sweep)

			notices := rig.notices(t)
			if len(notices) != 1 {
				t.Fatalf("the outbox holds %d volume-near-full notices, want exactly one", len(notices))
			}
			volume := notices[0].Volume
			if volume == nil {
				t.Fatalf("the notice carries no volume sweep: %+v", notices[0])
			}
			if !slices.Equal(volume.Sizes, sweep.Sizes) {
				t.Errorf("the notice carries sizes %+v, want the report's own %+v", volume.Sizes, sweep.Sizes)
			}
			if volume.SizesPartial != sweep.SizesPartial || !slices.Equal(volume.SizesMissing, sweep.SizesMissing) ||
				volume.SizesMissingOmitted != sweep.SizesMissingOmitted {
				t.Errorf("the notice carries a cut walk %t, missing %+v and %d left out, want the report's %t, %+v and %d",
					volume.SizesPartial, volume.SizesMissing, volume.SizesMissingOmitted, sweep.SizesPartial, sweep.SizesMissing, sweep.SizesMissingOmitted)
			}
			for _, size := range volume.Sizes {
				if (size.Refused != "") != (size.Path == pack) {
					t.Errorf("the size of %q carries the refused read %q, want one only where the walk read the size in part", size.Path, size.Refused)
				}
			}
		})
	}
}

// The bytes a sweep freed are its first reading's used bytes less its last's, which are negative
// when the volume's used bytes grew, and the notice carries that number as the report gave it, a
// negative one included: the architect's prompt reads it as the volume growing by that many bytes
// while the sweep ran, and the notice as a past sweep. The negative control is the sentence at
// b7085e21, which told "-1048576 freed by this sweep".
func TestAVolumeNoticeCarriesWhatTheSweepFreedAsTheReportGaveIt(t *testing.T) {
	for _, freed := range []int64{3 << 30, -1 << 20} {
		t.Run(fmt.Sprint(freed), func(t *testing.T) {
			rig := newVolumeRig(t)
			sweep := fullSweep()
			sweep.FreedBytes = freed
			rig.report(t, sweep)
			notices := rig.notices(t)
			if len(notices) != 1 || notices[0].Volume == nil {
				t.Fatalf("the outbox holds %+v, want one volume-near-full notice", notices)
			}
			if got := notices[0].Volume; got.FreedBytes != freed || got.UsedBytes != 48<<30 || got.FreeBytes != 2<<30 {
				t.Errorf("the notice carries used=%d free=%d freed=%d, want 51539607552, 2147483648 and %d, as the report gave them",
					got.UsedBytes, got.FreeBytes, got.FreedBytes, freed)
			}
		})
	}
}

// The switched-off trimmer's report is made by no sweep (launcher's switchOff), and its used and
// free bytes are the trimmer's last reading, and both zero where it made none. The notice carries
// them as the report gave them beside switchedOff, with the faults that stopped the trimmer and no
// size, and the architect's prompt says what of it is a sweep's and what is not: the episode, what
// raised it and the freed bytes say nothing of a sweep, and both readings at zero is a trimmer
// that made no reading. The negative control is the sentence at b7085e21, which read `0 bytes
// used, 0 free, 0 freed by this sweep` for a trimmer that made no reading.
func TestASwitchedOffNoticeCarriesTheLastReadingAsTheReportGaveIt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		used, free int64
	}{
		{"a trimmer that made no reading", 0, 0},
		{"a trimmer whose last reading stands", 49 << 30, 1 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off := record.VolumeNearFull{PodUID: volumePod, Trimmer: "trim-1", Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
				UsedBytes: tc.used, FreeBytes: tc.free, StopReason: "the trimmer switched itself off after 3 panics in a row",
				Faults: []string{"read the volume: panic: runtime error: invalid memory address\ngoroutine 9 [running]:"}, SwitchedOff: true}
			notice := VolumeNearFullNotice("LEGION-208", off)
			if notice.Kind != texts.VolumeNearFull || notice.Issue != "LEGION-208" || notice.Volume == nil {
				t.Fatalf("the notice is %+v, want LEGION-208's volume-near-full with the report", notice)
			}
			want := off
			want.Faults = []string{`read the volume: panic: runtime error: invalid memory address\ngoroutine 9 [running]:`}
			if got := *notice.Volume; !reflect.DeepEqual(got, want) {
				t.Errorf("the notice carries %+v, want the report as it came, each fault on one line: %+v", got, want)
			}
			if !strings.Contains(off.Faults[0], "\n") {
				t.Errorf("the notice wrote the faults of the caller's report: %q", off.Faults)
			}
			if strings.Contains(fmt.Sprint(notice), "freed by") {
				t.Errorf("the notice writes a sentence of a sweep: %+v", notice)
			}
		})
	}
}

// R2: "The fields are those of the kind, ... the versions and the state that the reader acts on
// that its event has" (dispatch://INFRA-3686/spec). The report of a trimmer that switched itself
// off is made by no sweep and measures no size, so it holds no time of a size walk; the notice must
// state none, as main's sentence left the time out where it was zero, rather than a walk measured
// at 0001-01-01, which the architect's prompt reads as the time the walk measured the sizes.
func TestASwitchedOffVolumeNoticeStatesNoTimeOfAWalk(t *testing.T) {
	notice := VolumeNearFullNotice("LEGION-208", record.VolumeNearFull{Trimmer: "t", Episode: 1, RaisedBy: "first", SwitchedOff: true,
		StopReason: "the trimmer switched itself off after 3 panics in a row of sweeps", Faults: []string{"panic: a", "panic: b", "panic: c"}})
	payload, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "0001-01-01") {
		t.Errorf("the notice of a switched-off trimmer states a size walk measured at year 1: %s", payload)
	}
}

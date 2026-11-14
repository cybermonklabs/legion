// The role claim copy's repair: what each whole read of the role bucket the role claim watcher
// makes repaired in the copy every claim view answers from (roleClaimRepair, which the copy's
// hooks in roleCache feed), and the report and bound /healthz and the grant guard read it by
// (RoleClaimRepair, RoleClaimRepairReport.Err, RoleClaimRepairBound). It is a concern of its own,
// split from roles.go the way role_renewal.go is: nothing here reads or writes the bucket, takes
// a role or gives one away, and the copy itself stays roleCache's.

package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// roleClaimRepairInterval is how long after each whole read of the role claims that completed the
// role claim watcher reads the bucket whole again (kvwatch.WithScheduledRead), which repairs a copy
// that drifted from its bucket in either direction: a claim the copy lost, and one it kept that the
// bucket no longer holds. A minute leaves room inside RoleClaimRepairBound for one whole read, the
// scan's idle timeout (kvwatch.ScanIdleTimeout, 10 s) and the 5 s a re-read whose setup failed
// waits before it is tried again, so a scheduled read that runs late still completes inside the
// bound, and /healthz and the grant guard do not flap at each read.
const roleClaimRepairInterval = time.Minute

// RoleClaimRepairBound is the longest the role claim copy may go without a whole read of the bucket
// that completed before its repair is reported late (RoleClaimRepairReport.Err): the live window
// (contracts.SessionLiveWindow, 5 minutes) less two 90-second heartbeats
// (contracts.SessionHeartbeat). A claim the copy lost is back within the bound, and its holder's
// next heartbeat renews it within one heartbeat more, so the claim is renewed again before the live
// window its last renewal opened ends and before another session may take the role. It is a
// constant and no setting: a setting would have to refuse every value above the bound, and no
// caller needs another value.
const RoleClaimRepairBound = contracts.SessionLiveWindow - 2*contracts.SessionHeartbeat

// roleClaimRepairLoggedRoles is how many roles the line a repairing read logs names, put back and
// taken out together: enough to name what one lost write or one missed delete left behind, and
// few enough that a read repairing a whole bucket's drift logs one line of bounded size.
const roleClaimRepairLoggedRoles = 10

// roleClaimRepair is what the role claim watcher's whole reads of the bucket repaired in the copy
// (roleCache), guarded by holdersMu. The counts of the read under way and those of the last read
// that completed are kept apart, so a read that begins and never completes - a scan that timed
// out, a read a rewatch replaced - leaves the last completed read's report as it was.
type roleClaimRepair struct {
	// repairing says the read under way repairs the copy and counts what it repairs (begin); it
	// is false between a completed read and the next begin, so a record the watcher delivers
	// while following counts nothing.
	repairing bool
	// sweeping is on only while the copy sweeps at the read's completion (roleCache.CompleteRead),
	// the one time an entry the copy removes is counted taken out by swept. The copy hands swept
	// every entry a delivered record or an Evict removes too: delivered counts the first, and the
	// second is this registry's own delete, which repairs nothing.
	sweeping bool
	// read is what the read under way has repaired so far.
	read roleClaimRepairCounts
	// completed is how many reads have completed, completedEpoch the epoch of the last of them
	// (kvwatch.Read.Epoch), completedAt when it completed on the registry's clock, and last what
	// it repaired.
	completed      int
	completedEpoch uint64
	completedAt    time.Time
	last           roleClaimRepairCounts
}

// begin starts the counts of read (roleCache.BeginRead). The read repairs the copy, and counts
// what it repairs, when a read of its epoch completed before it and it is not the first read of a
// recreated bucket: the copy it reads into was then filled by a complete read of that same bucket,
// so what it changes there is drift. The first read of an epoch, the start's or a recreated
// bucket's, fills the copy rather than repairing it and counts nothing, so a start does not report
// every claim of the bucket put back. The counts of the read under way start at zero here, apart
// from the last completed read's, which only complete moves.
func (repair *roleClaimRepair) begin(read kvwatch.Read) {
	repair.repairing = repair.completed > 0 && repair.completedEpoch == read.Epoch && !read.Recreated
	repair.read = roleClaimRepairCounts{}
}

// delivered counts what one record the read delivered (roleCache.Apply) did to role's entry: held
// and before are the entry before the record, holds and after the entry after it. A holder stored
// where the copy held none or held another is put back, and an entry the record removed - a
// delete marker, or a claim this build cannot read - is taken out. It counts nothing unless the
// read under way repairs.
func (repair *roleClaimRepair) delivered(role, before string, held bool, after string, holds bool) {
	if !repair.repairing {
		return
	}
	switch {
	case holds && (!held || after != before):
		repair.read.notePutBack(role)
	case held && !holds:
		repair.read.noteTookOut(role)
	}
}

// swept is the role claim copy's Rows.Removed (newRoleHolderCopy): it counts role's entry, which
// the copy removed, as taken out when the sweep at a repairing read's completion removed it
// (sweeping), since that read found no evidence the bucket still holds the claim.
func (repair *roleClaimRepair) swept(role, _ string) {
	if repair.sweeping && repair.repairing {
		repair.read.noteTookOut(role)
	}
}

// complete records read, whose sweep is done, as the last read that completed, at the moment at
// on the registry's clock, with what it repaired, which RoleClaimRepair reports and
// RoleClaimRepairReport.Err ages. It returns what the read repaired, for the line its completion
// logs.
func (repair *roleClaimRepair) complete(read kvwatch.Read, at time.Time) roleClaimRepairCounts {
	repair.repairing = false
	repair.completed++
	repair.completedEpoch = read.Epoch
	repair.completedAt = at
	repair.last = repair.read
	return repair.read
}

// roleClaimRepairCounts is what one read repaired: how many claims it put back and took out, and
// the first roleClaimRepairLoggedRoles of their roles, for the line its completion logs.
type roleClaimRepairCounts struct {
	putBack, tookOut           int
	putBackRoles, tookOutRoles []string
}

// notePutBack counts role's claim as one the read stored where the copy held none or held another
// holder.
func (c *roleClaimRepairCounts) notePutBack(role string) {
	c.putBack++
	if len(c.putBackRoles)+len(c.tookOutRoles) < roleClaimRepairLoggedRoles {
		c.putBackRoles = append(c.putBackRoles, role)
	}
}

// noteTookOut counts role's claim as one the read removed while the copy held it.
func (c *roleClaimRepairCounts) noteTookOut(role string) {
	c.tookOut++
	if len(c.putBackRoles)+len(c.tookOutRoles) < roleClaimRepairLoggedRoles {
		c.tookOutRoles = append(c.tookOutRoles, role)
	}
}

// RoleClaimRepairReport is what the role claim watcher's last whole read of the bucket that
// completed found in the copy every claim view answers from (RoleClaimRepair).
type RoleClaimRepairReport struct {
	// CompletedAt is when that read completed, on the registry's clock (WithClock), and zero when
	// no read has completed.
	CompletedAt time.Time
	// PutBack is how many claims that read stored where the copy held none or held another holder,
	// and TookOut how many it removed while the copy held them: by a delete marker it delivered, by
	// a claim it delivered that this build cannot read, which gives its role to nobody, or by the
	// sweep at its completion. Both are zero for the first read of an epoch, the start's or a
	// recreated bucket's, which fills the copy rather than repairing it. A claim another listener
	// wrote while the read ran can count as put back: the count does not tell that from drift.
	PutBack int
	TookOut int
	// Completed is how many whole reads of the bucket have completed since Open.
	Completed int
	// Age is how long before the report that read completed, on the registry's clock, and zero
	// when no read has completed. It is the age Err judges against RoleClaimRepairBound, so a
	// reader that reports it beside that error (/healthz) reports the very reading the error was
	// judged on.
	Age time.Duration
}

// RoleClaimRepair reports the role claim watcher's last whole read of the bucket that completed:
// when, how long ago, and what it put back and took out of the copy. Every read that completes
// counts - the start's, a rewatch's, a re-read after a timed-out scan or a recreate, and the
// scheduled read roleClaimRepairInterval after each of them - so the time is the copy's last repair
// however it came. It makes no bucket call, so a report of the repair never waits on the bus.
func (r *Registry) RoleClaimRepair() RoleClaimRepairReport {
	r.holdersMu.RLock()
	defer r.holdersMu.RUnlock()
	repair := &r.roleRepair
	report := RoleClaimRepairReport{
		CompletedAt: repair.completedAt,
		PutBack:     repair.last.putBack,
		TookOut:     repair.last.tookOut,
		Completed:   repair.completed,
	}
	if report.Completed > 0 {
		report.Age = r.now().Sub(report.CompletedAt)
	}
	return report
}

// Err is nil while the read report names completed no longer ago than RoleClaimRepairBound, as of
// the report, and otherwise an error that says no such read has completed, or how long ago the
// last one did. A copy past the bound may have lost a claim, or kept one the bucket no longer
// holds, for longer than the live window leaves its holder to renew it, so a caller that would
// decide on that copy reads this first. It is a report on the repair, not on whether the view
// answers (RoleViewErr): a copy that goes on following its bucket while no whole read completes -
// each scheduled read's setup failing, say - still answers RolesHeldBy past the bound.
func (report RoleClaimRepairReport) Err() error {
	if report.Completed == 0 {
		return errors.New("no whole read of the role claims has completed")
	}
	if report.Age > RoleClaimRepairBound {
		return fmt.Errorf("the last whole read of the role claims completed %s ago, past the %s bound",
			report.Age.Round(time.Millisecond), RoleClaimRepairBound)
	}
	return nil
}

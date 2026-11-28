// A role claim's grant: every grant writes an id of its own into the claim it makes
// (RoleClaim.GrantID), drawn at random and from no clock, which the holder's renewals, the adoption
// of a claim that names no process and the hand-back's mark carry over untouched. A hand-back
// whose mark lost its write, and a release whose delete lost its write, read the claim again and
// write again only while that id is the one they read, so a grant made since, in any millisecond,
// keeps the role it was just given. A claim the release before grant ids stored carries none, and
// it still renews, releases and is marked; a claim this release stored, which the release before
// rewrites without the field, is still marked and freed by the release that read it.

package store

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// earlierReleaseRoleClaim is the claim record as the release before grant ids reads and writes
// it: RoleClaim's stored fields without GrantID. That release decodes a record with encoding/json
// (DecodeRoleClaim), which skips a field it does not know, so it reads a record carrying a grant
// id; its renewal and its mark write back the claim they decoded, so the grant id is gone from
// any record it rewrites.
type earlierReleaseRoleClaim struct {
	HolderSessionID       string `json:"holder_session_id"`
	ClaimedAt             int64  `json:"claimed_at"`
	PreviousSessionID     string `json:"previous_session_id"`
	ClientInstanceID      string `json:"client_instance_id,omitempty"`
	RenewedAt             int64  `json:"renewed_at,omitempty"`
	RenewedWithoutProcess bool   `json:"renewed_without_process,omitempty"`
	HandedBack            bool   `json:"handed_back,omitempty"`
}

// writeAsTheEarlierRelease writes role's claim the way the release before grant ids does, through
// reg's role bucket and conditional on the revision it reads: change receives the record that
// release decoded and leaves the record it writes back.
func writeAsTheEarlierRelease(t *testing.T, reg *Registry, role string, change func(*earlierReleaseRoleClaim)) {
	t.Helper()
	kv := reg.roles().Legacy()
	entry, err := kv.Get(role)
	if err != nil {
		t.Fatalf("the earlier release's read of %s's claim: %v", role, err)
	}
	var claim earlierReleaseRoleClaim
	if err := json.Unmarshal(entry.Value(), &claim); err != nil {
		t.Fatalf("the earlier release decodes %s: %v", entry.Value(), err)
	}
	change(&claim)
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("encode the earlier release's claim %+v: %v", claim, err)
	}
	if _, err := kv.Update(role, data, entry.Revision()); err != nil {
		t.Fatalf("the earlier release's write of %s's claim: %v", role, err)
	}
}

// grantClock opens a registry over fresh buckets whose clock stands at noProcessClaimed until the
// returned advance moves it on, with holder's interest record carrying role's lane topic, so every
// registration of holder weighs role's claim and a release naming that topic hands it back.
func grantClock(t *testing.T, holder, role string) (*Registry, func(time.Duration)) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg, _ := coldRegistry(t, conn)
	var elapsed atomic.Int64
	reg.now = func() time.Time { return noProcessClaimed.Add(time.Duration(elapsed.Load())) }
	registerSessionWithLane(t, reg, holder, role)
	return reg, func(since time.Duration) { elapsed.Store(int64(since)) }
}

// Every grant writes a grant id, and a second grant in the very millisecond of the first, from
// the same process, writes another. The adoption of a claim that names no process, the holder's
// renewal, a renewal that states no process and the hand-back's mark each leave the id as the
// grant wrote it, with the claim's ClaimedAt.
func TestEveryGrantWritesAnIDOfItsOwnThatRenewalsTheAdoptionAndTheMarkKeep(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_grant_identity"
	)
	// The clock never moves, so every write below is stamped in one millisecond and nothing but
	// the grant id tells one grant from the next.
	reg, _ := grantClock(t, holder, role)

	claimRoleAs(t, reg, holder, "example-host", role, "")
	granted := storedRoleClaim(t, reg, role)
	if granted.GrantID == "" {
		t.Fatalf("the grant wrote %+v, want a grant id", granted)
	}
	keeps := func(step string) RoleClaim {
		t.Helper()
		stood := storedRoleClaim(t, reg, role)
		if stood.GrantID != granted.GrantID || stood.ClaimedAt != granted.ClaimedAt {
			t.Fatalf("the claim after %s is %+v, want the grant id %q and the moment %d of the grant it continues", step, stood, granted.GrantID, granted.ClaimedAt)
		}
		return stood
	}

	if renewal, err := renewRoleClaims(t, reg, holder, roleClaimInstance); err != nil || len(renewal.Renewed) != 1 {
		t.Fatalf("the first registration stating a process renewed %+v, %v; want %s's claim adopted", renewal, err, role)
	}
	if adopted := keeps("the adoption"); adopted.ClientInstanceID != roleClaimInstance {
		t.Fatalf("the adopted claim names the process %q, want %q", adopted.ClientInstanceID, roleClaimInstance)
	}
	if renewal, err := renewRoleClaims(t, reg, holder, roleClaimInstance); err != nil || len(renewal.Renewed) != 1 {
		t.Fatalf("the adopting process's renewal renewed %+v, %v; want %s's claim", renewal, err, role)
	}
	keeps("the holder's renewal")
	if _, err := reg.Remove(context.Background(), holder, []string{contracts.RoleTopicPrefix + role}, RoleReleaseAuthority{}); err != nil {
		t.Fatalf("the hand-back stating no process: %v", err)
	}
	if marked := keeps("the hand-back"); !marked.HandedBack {
		t.Fatalf("the claim after the hand-back is %+v, want it marked handed back", marked)
	}

	claimRoleAs(t, reg, holder, "example-host", role, roleClaimInstance)
	regranted := storedRoleClaim(t, reg, role)
	if regranted.GrantID == "" || regranted.GrantID == granted.GrantID || regranted.ClaimedAt != granted.ClaimedAt || regranted.HandedBack {
		t.Fatalf("the grant to the same process in the same millisecond wrote %+v, want an unmarked claim of %d with a grant id other than %q", regranted, granted.ClaimedAt, granted.GrantID)
	}

	claimRoleAs(t, reg, holder, "example-host", role, "")
	granted = storedRoleClaim(t, reg, role)
	if renewal, err := renewRoleClaims(t, reg, holder, ""); err != nil || len(renewal.Renewed) != 1 {
		t.Fatalf("a registration stating no process renewed %+v, %v; want %s's claim", renewal, err, role)
	}
	if renewed := keeps("a renewal that states no process"); !renewed.RenewedWithoutProcess {
		t.Fatalf("the claim after a renewal that states no process is %+v, want that renewal noted", renewed)
	}
}

// A claim the release before grant ids stored carries no grant id, and this release reads it as
// that release did. Its own process renews it, and the renewal adds no grant id; its own
// process's release frees it. A hand-back that states no process marks one that names no process,
// also when the holder's renewal or the adoption by its session's first heartbeat lands under the
// mark's first write, since neither adds a grant id. What tells the claim from a grant made since
// is that grant's id where the grant is this release's, which no millisecond can hide, and the
// moment it was claimed at where the earlier release made it, as that release did.
func TestAClaimTheEarlierReleaseStoredRenewsReleasesAndIsMarked(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_claim_without_grant_id"
		beat   = 20 * time.Second
	)
	lane := contracts.RoleTopicPrefix + role
	// storeEarlierClaim stores the record the release before grant ids writes for holder's
	// grant through the process instance names (none where empty), at noProcessClaimed.
	storeEarlierClaim := func(t *testing.T, instance string) (*Registry, func(time.Duration), RoleClaim) {
		t.Helper()
		reg, advance := grantClock(t, holder, role)
		record, err := json.Marshal(earlierReleaseRoleClaim{HolderSessionID: holder, ClaimedAt: noProcessClaimed.UnixMilli(), ClientInstanceID: instance, RenewedAt: noProcessClaimed.UnixMilli()})
		if err != nil {
			t.Fatalf("encode the earlier release's claim: %v", err)
		}
		if _, err := reg.roles().Legacy().Put(role, record); err != nil {
			t.Fatalf("store the earlier release's claim %s: %v", record, err)
		}
		stored := storedRoleClaim(t, reg, role)
		if stored.GrantID != "" || stored.HolderSessionID != holder || stored.ClientInstanceID != instance {
			t.Fatalf("the stored claim reads %+v, want %s's claim naming %q and no grant id", stored, holder, instance)
		}
		return reg, advance, stored
	}

	t.Run("a claim that names a process is renewed by that process and freed by its release", func(t *testing.T) {
		reg, advance, stored := storeEarlierClaim(t, roleClaimInstance)

		advance(beat)
		if renewal, err := renewRoleClaims(t, reg, holder, roleClaimInstance); err != nil || len(renewal.Renewed) != 1 {
			t.Fatalf("the holder's registration renewed %+v, %v; want %s's claim", renewal, err, role)
		}
		renewed := storedRoleClaim(t, reg, role)
		if want := stored.ClaimedAt + beat.Milliseconds(); renewed.GrantID != "" || renewed.ClaimedAt != stored.ClaimedAt || renewed.RenewedAt != want || renewed.ClientInstanceID != roleClaimInstance {
			t.Fatalf("the claim after the holder's renewal is %+v, want the claim of %d renewed at %d, naming %q and still no grant id", renewed, stored.ClaimedAt, want, roleClaimInstance)
		}

		if _, err := reg.Remove(context.Background(), holder, []string{lane}, releasedByItsProcess); err != nil {
			t.Fatalf("the holder's own release: %v", err)
		}
		assertClaimFreed(t, reg, role)
	})

	t.Run("a claim that names no process", func(t *testing.T) {
		t.Run("a hand-back that states no process marks it", func(t *testing.T) {
			reg, _, stored := storeEarlierClaim(t, "")

			if _, err := reg.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{}); err != nil {
				t.Fatalf("the hand-back: %v", err)
			}
			marked := storedRoleClaim(t, reg, role)
			want := stored
			want.HandedBack, want.Revision = true, marked.Revision
			if marked.Revision == stored.Revision || marked != want {
				t.Fatalf("the claim after the hand-back is %+v, want %+v marked handed back and nothing else changed", marked, stored)
			}
		})

		t.Run("a hand-back that races the holder's renewal marks the renewed claim", func(t *testing.T) {
			reg, advance, stored := storeEarlierClaim(t, "")
			advance(beat)
			racing := raceFirstMarkWith(reg, func() {
				if _, err := reg.renewRoleClaim(context.Background(), role, holder, ""); err != nil {
					t.Errorf("the holder's renewal that wins the race: %v", err)
				}
			})

			if _, err := reg.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{}); err != nil {
				t.Fatalf("the hand-back: %v", err)
			}

			if racing.updates < 2 {
				t.Fatalf("the hand-back sent %d conditional writes, want its first lost to the renewal and a second", racing.updates)
			}
			stood := storedRoleClaim(t, reg, role)
			if want := stored.ClaimedAt + beat.Milliseconds(); !stood.HandedBack || stood.GrantID != "" || stood.RenewedAt != want || stood.ClaimedAt != stored.ClaimedAt {
				t.Fatalf("the claim after the hand-back is %+v, want the claim the renewal left (renewed at %d, no grant id) marked handed back", stood, want)
			}
		})

		t.Run("a hand-back that races the adoption by the first heartbeat marks the adopted claim", func(t *testing.T) {
			reg, advance, stored := storeEarlierClaim(t, "")
			advance(beat)
			racing := raceFirstMarkWith(reg, func() {
				if _, err := reg.renewRoleClaim(context.Background(), role, holder, roleClaimInstance); err != nil {
					t.Errorf("the first heartbeat that adopts the claim: %v", err)
				}
			})

			if _, err := reg.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{}); err != nil {
				t.Fatalf("the hand-back: %v", err)
			}

			if racing.updates < 2 {
				t.Fatalf("the hand-back sent %d conditional writes, want its first lost to the adoption and a second", racing.updates)
			}
			stood := storedRoleClaim(t, reg, role)
			if !stood.HandedBack || stood.ClientInstanceID != roleClaimInstance || stood.GrantID != "" || stood.ClaimedAt != stored.ClaimedAt {
				t.Fatalf("the claim after the hand-back is %+v, want the claim adopted by %q, still with no grant id, marked handed back", stood, roleClaimInstance)
			}
		})

		for name, grant := range map[string]func(t *testing.T, reg *Registry, advance func(time.Duration)){
			"a hand-back that races this release's grant in the same millisecond leaves the grant standing and unmarked": func(t *testing.T, reg *Registry, _ func(time.Duration)) {
				claimRoleAs(t, reg, holder, "example-host", role, "")
			},
			"a hand-back that races the earlier release's grant in a later millisecond leaves the grant standing and unmarked": func(t *testing.T, reg *Registry, advance func(time.Duration)) {
				advance(time.Hour)
				writeAsTheEarlierRelease(t, reg, role, func(claim *earlierReleaseRoleClaim) {
					later := noProcessClaimed.Add(time.Hour).UnixMilli()
					*claim = earlierReleaseRoleClaim{HolderSessionID: holder, ClaimedAt: later, RenewedAt: later}
				})
			},
		} {
			t.Run(name, func(t *testing.T) {
				reg, advance, _ := storeEarlierClaim(t, "")
				var granted RoleClaim
				racing := raceFirstMarkWith(reg, func() {
					grant(t, reg, advance)
					granted = storedRoleClaim(t, reg, role)
				})

				if _, err := reg.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{}); err != nil {
					t.Fatalf("the hand-back: %v", err)
				}

				if racing.updates != 1 {
					t.Fatalf("the hand-back sent %d conditional writes, want one: a claim granted since the read is never marked", racing.updates)
				}
				if stood := storedRoleClaim(t, reg, role); stood != granted || stood.HandedBack {
					t.Fatalf("the claim after the hand-back is %+v, want the grant %+v standing and unmarked", stood, granted)
				}
			})
		}
	})
}

// A claim this release granted and the release before grant ids then renewed carries no grant
// id any more: that release writes back the record it decoded, which has no such field. While the
// two releases share the bucket, a hand-back whose mark lost its write to that renewal reads the
// claim again and still marks it, since its moment and its process are the ones the hand-back
// read. Control: a grant the earlier release made in a later millisecond is left standing and
// unmarked.
func TestAHandBackMarksAClaimTheEarlierReleaseRenewed(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_grant_renewed_by_earlier_release"
		beat   = 20 * time.Second
	)
	lane := contracts.RoleTopicPrefix + role

	t.Run("the earlier release's renewal under the mark's first write is marked", func(t *testing.T) {
		reg, advance := grantClock(t, holder, role)
		claimRoleAs(t, reg, holder, "example-host", role, roleClaimInstance)
		granted := storedRoleClaim(t, reg, role)
		advance(beat)
		racing := raceFirstMarkWith(reg, func() {
			writeAsTheEarlierRelease(t, reg, role, func(claim *earlierReleaseRoleClaim) {
				claim.RenewedAt = noProcessClaimed.Add(beat).UnixMilli()
			})
		})

		if _, err := reg.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{}); err != nil {
			t.Fatalf("the hand-back: %v", err)
		}

		if racing.updates < 2 {
			t.Fatalf("the hand-back sent %d conditional writes, want its first lost to the earlier release's renewal and a second", racing.updates)
		}
		stood := storedRoleClaim(t, reg, role)
		if want := granted.ClaimedAt + beat.Milliseconds(); !stood.HandedBack || stood.GrantID != "" || stood.RenewedAt != want || stood.ClaimedAt != granted.ClaimedAt || stood.ClientInstanceID != roleClaimInstance {
			t.Fatalf("the claim after the hand-back is %+v, want the earlier release's renewal of %+v (renewed at %d, its grant id dropped) marked handed back", stood, granted, want)
		}
	})

	t.Run("control: the earlier release's grant in a later millisecond is left standing and unmarked", func(t *testing.T) {
		reg, advance := grantClock(t, holder, role)
		claimRoleAs(t, reg, holder, "example-host", role, roleClaimInstance)
		var granted RoleClaim
		racing := raceFirstMarkWith(reg, func() {
			advance(time.Hour)
			writeAsTheEarlierRelease(t, reg, role, func(claim *earlierReleaseRoleClaim) {
				later := noProcessClaimed.Add(time.Hour).UnixMilli()
				*claim = earlierReleaseRoleClaim{HolderSessionID: holder, ClaimedAt: later, ClientInstanceID: roleClaimInstance, RenewedAt: later}
			})
			granted = storedRoleClaim(t, reg, role)
		})

		if _, err := reg.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{}); err != nil {
			t.Fatalf("the hand-back: %v", err)
		}

		if racing.updates != 1 {
			t.Fatalf("the hand-back sent %d conditional writes, want one: a claim granted since the read is never marked", racing.updates)
		}
		if stood := storedRoleClaim(t, reg, role); stood != granted || stood.HandedBack {
			t.Fatalf("the claim after the hand-back is %+v, want the earlier release's grant %+v standing and unmarked", stood, granted)
		}
	})
}

// The delete's side of the same race: a claim this release granted, which the release before
// grant ids renews between a release's read of the claim and its conditional delete, dropping the
// grant id. The delete loses its condition and reads the claim again, and the record it finds
// carries no grant id, so its moment and its process are what say it is the claim the release
// read (RoleClaim.sameGrant, asked of the record read again): the release sends its delete again
// and frees the claim, under the holder's own release and the administrator's force alike.
// Control: a grant the earlier release made in a later millisecond is left standing.
func TestAReleaseFreesAClaimTheEarlierReleaseRenewedUnderItsDelete(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_released_under_an_earlier_release_renewal"
		beat   = 20 * time.Second
	)
	lane := contracts.RoleTopicPrefix + role
	for name, authority := range map[string]RoleReleaseAuthority{
		"the holder's own process":  releasedByItsProcess,
		"the administrator's force": {Force: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("the earlier release's renewal under the delete is freed", func(t *testing.T) {
				reg, advance := grantClock(t, holder, role)
				claimRoleAs(t, reg, holder, "example-host", role, roleClaimInstance)
				read := storedRoleClaim(t, reg, role)
				advance(beat)
				var renewed RoleClaim
				racing := raceReleaseWith(t, reg, role, 1, func() {
					writeAsTheEarlierRelease(t, reg, role, func(claim *earlierReleaseRoleClaim) {
						claim.RenewedAt = noProcessClaimed.Add(beat).UnixMilli()
					})
					renewed = storedRoleClaim(t, reg, role)
				})

				if _, err := reg.Remove(context.Background(), holder, []string{lane}, authority); err != nil {
					t.Fatalf("the release: %v", err)
				}

				if read.GrantID == "" || renewed.GrantID != "" || renewed.ClaimedAt != read.ClaimedAt {
					t.Fatalf("the claim the release read is %+v and the earlier release's renewal %+v, want a grant id and then none, at the same moment", read, renewed)
				}
				if racing.deletes != 2 {
					t.Fatalf("the release sent %d conditional deletes, want two: the one the earlier release's renewal beat and the one at the revision it left", racing.deletes)
				}
				assertClaimFreed(t, reg, role)
			})

			t.Run("control: the earlier release's grant in a later millisecond is left standing", func(t *testing.T) {
				reg, advance := grantClock(t, holder, role)
				claimRoleAs(t, reg, holder, "example-host", role, roleClaimInstance)
				var granted RoleClaim
				racing := raceReleaseWith(t, reg, role, 1, func() {
					advance(time.Hour)
					writeAsTheEarlierRelease(t, reg, role, func(claim *earlierReleaseRoleClaim) {
						later := noProcessClaimed.Add(time.Hour).UnixMilli()
						*claim = earlierReleaseRoleClaim{HolderSessionID: holder, ClaimedAt: later, ClientInstanceID: roleClaimInstance, RenewedAt: later}
					})
					granted = storedRoleClaim(t, reg, role)
				})

				if _, err := reg.Remove(context.Background(), holder, []string{lane}, authority); err != nil {
					t.Fatalf("the release: %v", err)
				}

				if racing.deletes != 1 {
					t.Fatalf("the release sent %d conditional deletes, want one: a claim granted since the read is never deleted", racing.deletes)
				}
				if stood := storedRoleClaim(t, reg, role); stood != granted {
					t.Fatalf("the claim after the release is %+v, want the earlier release's grant %+v untouched", stood, granted)
				}
			})
		})
	}
}

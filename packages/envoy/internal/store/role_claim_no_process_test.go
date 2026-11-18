// A role claim that names no process: one a client wrote that states none, which is a client of
// an older build, or a session pod's claim of its launch role (the pod's role claimer, which
// sends POST /v1/roles/set with no client_instance_id). In a release an absent process matches
// nothing, so no release but the administrator's force frees such a claim; a hand-back that
// states no process marks it, as it marks a claim that names one; and a registration that states
// a process adopts it only at the first heartbeat after it was written, before any registration
// that states no process has renewed it.

package store

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// noProcessClaimed is the moment every claim below is written at, on its registry's clock.
var noProcessClaimed = time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)

// noProcessLiveWindow is the live window the role claim reaper is given below.
const noProcessLiveWindow = time.Minute

// claimReleasePath is one of the three ways a release reaches a session's role claims.
type claimReleasePath struct {
	name string
	// handsBack names the role's own lane topic, which is a client's hand-back of that one
	// role (releaseRoleClaims). Without it the release names no topic and is the session's
	// deregistration (releaseAllRoleClaims).
	handsBack bool
	// viewed opens the registry with a role claim view that answers (Open), so a deregistration
	// releases the roles that view names. Without it the view cannot be served (coldRegistry),
	// and a deregistration falls back to the scan of the role bucket
	// (releaseAllRoleClaimsByScan).
	viewed bool
}

var claimReleasePaths = []claimReleasePath{
	{name: "the release of named topics", handsBack: true},
	{name: "a deregistration through the role view", viewed: true},
	{name: "the scan that stands in for the view"},
}

// topics is what a release on this path names: the role's lane topic for a hand-back, and none
// for a deregistration.
func (p claimReleasePath) topics(role string) []string {
	if p.handsBack {
		return []string{contracts.RoleTopicPrefix + role}
	}
	return nil
}

// claimForRelease opens a registry over a fresh pair of buckets for path, with holder's interest
// record carrying a topic of its own, and gives holder role through the process instance names
// (none where instance is empty). For a deregistration the role's lane topic is taken back off
// the record, so what names the claim to the release is the view, or the scan, alone. The
// returned advance moves the registry's clock that far past the claim.
func claimForRelease(t *testing.T, path claimReleasePath, role, holder, instance string) (*Registry, func(time.Duration)) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	var elapsed atomic.Int64
	clock := func() time.Time { return noProcessClaimed.Add(time.Duration(elapsed.Load())) }
	var reg *Registry
	if path.viewed {
		opened, err := Open(conn, WithReplicas(1), WithClock(clock))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(opened.StopWatch)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := opened.WaitForCacheReady(ctx); err != nil {
			t.Fatalf("WaitForCacheReady: %v", err)
		}
		reg = opened
	} else {
		reg, _ = coldRegistry(t, conn)
		reg.now = clock
	}
	if _, err := reg.Upsert(Interest{SessionID: holder, MachineID: "example-host"}, []string{releaseKeptTopic}); err != nil {
		t.Fatalf("register %s's interests: %v", holder, err)
	}
	claimRoleAs(t, reg, holder, "example-host", role, instance)
	if !path.handsBack {
		if err := reg.removeInterestTopics(context.Background(), holder, []string{contracts.RoleTopicPrefix + role}); err != nil {
			t.Fatalf("take %s's lane topic off %s's record: %v", role, holder, err)
		}
		assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic)
	}
	if path.viewed {
		waitForHeld(t, reg, holder, role)
	}
	if claim := storedRoleClaim(t, reg, role); claim.HolderSessionID != holder || claim.ClientInstanceID != instance {
		t.Fatalf("the seeded claim is %+v, want %s's claim naming the process %q", claim, holder, instance)
	}
	return reg, func(since time.Duration) { elapsed.Store(int64(since)) }
}

// releaseClaimOn sends path's release of holder's claim of role with authority.
func releaseClaimOn(t *testing.T, reg *Registry, path claimReleasePath, holder, role string, authority RoleReleaseAuthority) {
	t.Helper()
	if _, err := reg.Remove(context.Background(), holder, path.topics(role), authority); err != nil {
		t.Fatalf("%s of %s's claim of %s by %+v: %v", path.name, holder, role, authority, err)
	}
}

// assertClaimFreed fails the test unless role has no claim record at all.
func assertClaimFreed(t *testing.T, reg *Registry, role string) {
	t.Helper()
	if claim := storedRoleClaim(t, reg, role); claim.Revision != 0 {
		t.Fatalf("the claim stands at revision %d as %+v, want it freed", claim.Revision, claim)
	}
}

// raceFirstMarkWith lands the write wins makes between a hand-back's read of the claim and the
// first conditional update it sends, the write of its mark: a hand-back states no process and
// frees nothing, so that update is its first write (racingUpdateKeyValue hooks the first update
// alone, and wins's own writes are not hooked again).
func raceFirstMarkWith(reg *Registry, wins func()) *racingUpdateKeyValue {
	racing := &racingUpdateKeyValue{KeyValue: reg.roleKV.Legacy(), beforeFirstUpdate: wins}
	reg.roleKV = bus.NewKeyValue(racing, reg.roleKV.WatchHandle(), reg.roleKV.Streams())
	return racing
}

// No release but the administrator's force frees a claim that names no process, on each of the
// three release paths: a release or a deregistration that states a process frees nothing and
// marks nothing, a deregistration that states none frees nothing, and a hand-back that states
// none marks the claim. No registration renews a marked claim, and the role claim reaper frees
// it once its renewal is older than the live window, with its session alive to the reaper the
// whole time. A hand-back that states no process and races the holder's renewal, the adoption
// of the claim by the first heartbeat, or a fresh grant marks the renewed or adopted claim and
// leaves the grant standing and unmarked. Control: a claim that names a process is freed by a
// release that states that process and by nothing another process states.
func TestARoleClaimThatNamesNoProcess(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_claim_names_no_process"
		beat   = 20 * time.Second
	)
	lane := contracts.RoleTopicPrefix + role
	for _, path := range claimReleasePaths {
		t.Run(path.name, func(t *testing.T) {
			t.Run("a release by a process that states its own frees nothing and marks nothing", func(t *testing.T) {
				reg, _ := claimForRelease(t, path, role, holder, "")
				seeded := storedRoleClaim(t, reg, role)

				releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance})

				if stood := storedRoleClaim(t, reg, role); stood != seeded {
					t.Fatalf("the claim after the release is %+v, want it exactly as it stood, %+v: a release that states a process frees no claim that names none, and marks none", stood, seeded)
				}
				if path.handsBack {
					assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic, lane)
				}
			})

			if path.handsBack {
				t.Run("a hand-back that states no process marks the claim, no registration renews it, and the reaper frees it past the live window while its session registers", func(t *testing.T) {
					reg, advance := claimForRelease(t, path, role, holder, "")
					seeded := storedRoleClaim(t, reg, role)
					alive := func(string) bool { return true }

					releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{})

					marked := storedRoleClaim(t, reg, role)
					want := seeded
					want.HandedBack, want.Revision = true, marked.Revision
					if marked.Revision == seeded.Revision || marked != want {
						t.Fatalf("the claim after the hand-back is %+v (revision %d), want %+v marked handed back and nothing else changed", marked, marked.Revision, seeded)
					}
					assertInterestTopicsForSession(t, reg, holder, releaseKeptTopic, lane)

					for _, since := range []time.Duration{beat, noProcessLiveWindow - beat} {
						advance(since)
						for _, instance := range []string{"", roleClaimInstance, roleClaimSecondInstance} {
							if _, err := renewRoleClaims(t, reg, holder, instance); err != nil {
								t.Fatalf("a registration %s after the hand-back by process %q: %v", since, instance, err)
							}
							if stood := storedRoleClaim(t, reg, role); stood != marked {
								t.Fatalf("the claim %s after the hand-back, registered by process %q, is %+v, want it exactly as the hand-back left it, %+v", since, instance, stood, marked)
							}
						}
						if sweep, err := reg.ReapRoleClaims(context.Background(), alive, 0, noProcessLiveWindow); err != nil || sweep.Reaped != 0 {
							t.Fatalf("a reaper pass %s after the hand-back freed %+v, %v; want none inside the live window", since, sweep, err)
						}
						assertRoleHolder(t, reg, role, holder)
					}

					advance(noProcessLiveWindow + beat)
					sweep, err := reg.ReapRoleClaims(context.Background(), alive, 0, noProcessLiveWindow)
					if err != nil || sweep.Reaped != 1 {
						t.Fatalf("the reaper pass past the live window freed %+v, %v; want the one marked claim, its session alive", sweep, err)
					}
					assertRoleHolder(t, reg, role, "")
				})

				t.Run("a hand-back that states no process and races", func(t *testing.T) {
					t.Run("the holder's renewal marks the renewed claim", func(t *testing.T) {
						reg, advance := claimForRelease(t, path, role, holder, "")
						seeded := storedRoleClaim(t, reg, role)
						advance(beat)
						racing := raceFirstMarkWith(reg, func() {
							if _, err := reg.renewRoleClaim(context.Background(), role, holder, ""); err != nil {
								t.Errorf("the holder's renewal that wins the race: %v", err)
							}
						})

						releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{})

						stood := storedRoleClaim(t, reg, role)
						if racing.updates == 0 {
							t.Fatalf("the hand-back sent no write for the renewal to race")
						}
						if want := seeded.ClaimedAt + beat.Milliseconds(); !stood.HandedBack || stood.ClientInstanceID != "" || stood.RenewedAt != want || stood.ClaimedAt != seeded.ClaimedAt {
							t.Fatalf("the claim after the hand-back is %+v (revision %d), want the claim the renewal left (renewed at %d, naming no process) marked handed back", stood, stood.Revision, want)
						}
					})
					t.Run("an adoption by the first heartbeat marks the adopted claim", func(t *testing.T) {
						reg, advance := claimForRelease(t, path, role, holder, "")
						seeded := storedRoleClaim(t, reg, role)
						advance(beat)
						racing := raceFirstMarkWith(reg, func() {
							if _, err := reg.renewRoleClaim(context.Background(), role, holder, roleClaimInstance); err != nil {
								t.Errorf("the first heartbeat that adopts the claim: %v", err)
							}
						})

						releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{})

						stood := storedRoleClaim(t, reg, role)
						if racing.updates == 0 {
							t.Fatalf("the hand-back sent no write for the adoption to race")
						}
						if !stood.HandedBack || stood.ClientInstanceID != roleClaimInstance || stood.ClaimedAt != seeded.ClaimedAt {
							t.Fatalf("the claim after the hand-back is %+v (revision %d), want the claim adopted by %q marked handed back", stood, stood.Revision, roleClaimInstance)
						}
					})
					for name, instance := range map[string]string{
						"a fresh grant naming no process leaves the grant standing and unmarked":                      "",
						"a fresh grant naming a process leaves the grant standing and unmarked":                       roleClaimInstance,
						"a fresh grant naming a second process of the session leaves the grant standing and unmarked": roleClaimSecondInstance,
					} {
						t.Run(name, func(t *testing.T) {
							reg, advance := claimForRelease(t, path, role, holder, "")
							seeded := storedRoleClaim(t, reg, role)
							var granted RoleClaim
							racing := raceFirstMarkWith(reg, func() {
								// An hour on, so the grant's moment is one the claim the
								// hand-back read cannot share.
								advance(time.Hour)
								claimRoleAs(t, reg, holder, "example-host", role, instance)
								granted = storedRoleClaim(t, reg, role)
							})

							releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{})

							if racing.updates == 0 {
								t.Fatalf("the hand-back sent no write for the grant to race")
							}
							if granted.ClaimedAt <= seeded.ClaimedAt {
								t.Fatalf("the grant was claimed at %d and the claim the hand-back read at %d, want the grant's moment past it", granted.ClaimedAt, seeded.ClaimedAt)
							}
							if stood := storedRoleClaim(t, reg, role); stood != granted {
								t.Fatalf("the claim after the hand-back is %+v (revision %d), want the grant %+v (revision %d) standing and unmarked", stood, stood.Revision, granted, granted.Revision)
							}
						})
					}
				})
			} else {
				t.Run("a deregistration that states no process frees nothing", func(t *testing.T) {
					reg, _ := claimForRelease(t, path, role, holder, "")
					seeded := storedRoleClaim(t, reg, role)

					releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{})

					if stood := storedRoleClaim(t, reg, role); stood != seeded {
						t.Fatalf("the claim after the deregistration is %+v, want it exactly as it stood, %+v: a deregistration that states no process frees nothing, and marks nothing", stood, seeded)
					}
					assertInterestMissing(t, reg, holder)
				})
			}

			t.Run("the administrator's force frees it", func(t *testing.T) {
				reg, _ := claimForRelease(t, path, role, holder, "")

				releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{Force: true})

				assertClaimFreed(t, reg, role)
			})

			t.Run("control: a claim that names a process", func(t *testing.T) {
				t.Run("a release that states that process frees it", func(t *testing.T) {
					reg, _ := claimForRelease(t, path, role, holder, roleClaimInstance)

					releaseClaimOn(t, reg, path, holder, role, releasedByItsProcess)

					assertClaimFreed(t, reg, role)
				})
				t.Run("a release that states another process leaves it", func(t *testing.T) {
					reg, _ := claimForRelease(t, path, role, holder, roleClaimInstance)
					seeded := storedRoleClaim(t, reg, role)

					releaseClaimOn(t, reg, path, holder, role, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance})

					if stood := storedRoleClaim(t, reg, role); stood != seeded {
						t.Fatalf("the claim after another process's release is %+v, want it exactly as it stood, %+v", stood, seeded)
					}
				})
			})
		})
	}
}

// adoptionClock is one way the clocks of the two listeners a session's registrations reach can
// read against the moment its claim was written: a renewal written after it, at it, or before
// it. A claim's renewal time is therefore no evidence of whether the claim was renewed.
type adoptionClock struct {
	name string
	// moves is whether the claim's listener's clock moves on between registrations.
	moves bool
	// behind is how far the second listener's clock is behind the first's.
	behind time.Duration
	// renewOnSecond sends the registration that renews or adopts the claim to the second
	// listener rather than the one the claim was written through.
	renewOnSecond bool
}

var adoptionClocks = []adoptionClock{
	{name: "a clock that moves on", moves: true},
	{name: "a clock that does not move"},
	{name: "a second listener whose clock is behind", moves: true, behind: time.Hour, renewOnSecond: true},
}

// adoptionListeners opens two registries over one fresh pair of buckets, standing for two
// listeners that serve one session's registrations: first, whose clock the claim is written on,
// and second, whose clock is clock.behind behind it. Neither has a role claim view, so a
// registration weighs the roles of the session's interest record. renewer is the listener the
// registration that renews or adopts the claim goes to: second where clock.renewOnSecond, else
// first. The returned advance moves both clocks on, where clock.moves says they move.
func adoptionListeners(t *testing.T, clock adoptionClock) (first, second, renewer *Registry, advance func(time.Duration)) {
	t.Helper()
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	var elapsed atomic.Int64
	first, _ = coldRegistry(t, conn)
	first.now = func() time.Time { return noProcessClaimed.Add(time.Duration(elapsed.Load())) }
	second, _ = coldRegistry(t, conn)
	second.now = func() time.Time { return noProcessClaimed.Add(time.Duration(elapsed.Load()) - clock.behind) }
	renewer = first
	if clock.renewOnSecond {
		renewer = second
	}
	return first, second, renewer, func(since time.Duration) {
		if clock.moves {
			elapsed.Store(int64(since))
		}
	}
}

// registerSessionWithLane stores holder's interest record carrying role's lane topic beside a
// topic of its own, as a grant leaves it, so every registration of holder weighs role's claim.
func registerSessionWithLane(t *testing.T, reg *Registry, holder, role string) {
	t.Helper()
	if _, err := reg.Upsert(Interest{SessionID: holder, MachineID: "example-host"}, []string{releaseKeptTopic, contracts.RoleTopicPrefix + role}); err != nil {
		t.Fatalf("register %s's interests: %v", holder, err)
	}
}

// A claim that names no process is adopted by a registration that states a process only while
// no registration has renewed it since it was written. One that a registration stating no
// process renewed still names no process, keeps that renewal when a second process of the
// session registers stating its own, and is marked by its holder's own hand-back after that. One
// that nothing renewed is adopted by the first registration of its session that states a
// process, and from then on only that process renews or frees it; a claim an earlier build
// stored, which carries no record of such a renewal, is adopted the same way. Each holds when
// the renewal's time equals the claim's (a clock that does not move) or is before it (a second
// listener whose clock is behind), and a second registry that reads the stored record decides
// the same way.
func TestARoleClaimThatNamesNoProcessIsAdoptedOnlyAtItsFirstHeartbeat(t *testing.T) {
	const (
		role   = "legion-controller"
		holder = "ses_claim_adopted_once"
		beat   = 20 * time.Second
	)
	lane := contracts.RoleTopicPrefix + role
	for _, clock := range adoptionClocks {
		t.Run(clock.name, func(t *testing.T) {
			t.Run("a claim a registration stating no process renewed is not taken by a second process", func(t *testing.T) {
				first, second, renewer, advance := adoptionListeners(t, clock)
				registerSessionWithLane(t, first, holder, role)
				claimRoleAs(t, first, holder, "example-host", role, "")
				claimed := storedRoleClaim(t, first, role)

				advance(beat)
				if renewal, err := renewRoleClaims(t, renewer, holder, ""); err != nil || len(renewal.Renewed) != 1 {
					t.Fatalf("the holder's registration stating no process renewed %+v, %v; want %s's claim renewed", renewal, err, role)
				}
				renewed := storedRoleClaim(t, first, role)
				if renewed.ClientInstanceID != "" || renewed.ClaimedAt != claimed.ClaimedAt || renewed.Revision == claimed.Revision {
					t.Fatalf("the claim after the holder's renewal is %+v (revision %d), want the claim of %d renewed and naming no process", renewed, renewed.Revision, claimed.ClaimedAt)
				}

				advance(2 * beat)
				for name, listener := range map[string]*Registry{"the first listener": first, "the second listener": second} {
					if _, err := renewRoleClaims(t, listener, holder, roleClaimSecondInstance); err != nil {
						t.Fatalf("the second process's registration through %s: %v", name, err)
					}
					if stood := storedRoleClaim(t, first, role); stood != renewed {
						t.Fatalf("the claim after the second process's registration through %s is %+v, want it exactly as the holder's renewal left it, %+v: a claim renewed since it was written is adopted by no registration", name, stood, renewed)
					}
				}

				if _, err := first.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{}); err != nil {
					t.Fatalf("the holder's own hand-back stating no process: %v", err)
				}
				marked := storedRoleClaim(t, first, role)
				if !marked.HandedBack || marked.HolderSessionID != holder || marked.ClientInstanceID != "" {
					t.Fatalf("the claim after the holder's own hand-back is %+v, want it marked handed back, naming no process", marked)
				}
			})

			t.Run("control: a claim no registration renewed is adopted by the first registration stating a process", func(t *testing.T) {
				first, second, adopter, advance := adoptionListeners(t, clock)
				registerSessionWithLane(t, first, holder, role)
				claimRoleAs(t, first, holder, "example-host", role, "")
				claimed := storedRoleClaim(t, first, role)

				advance(beat)
				if _, err := renewRoleClaims(t, adopter, holder, roleClaimInstance); err != nil {
					t.Fatalf("the first registration stating a process: %v", err)
				}
				adopted := storedRoleClaim(t, first, role)
				if adopted.ClientInstanceID != roleClaimInstance || adopted.HolderSessionID != holder || adopted.ClaimedAt != claimed.ClaimedAt {
					t.Fatalf("the claim after the first registration stating a process is %+v, want %s's claim of %d adopted by %q", adopted, holder, claimed.ClaimedAt, roleClaimInstance)
				}

				advance(2 * beat)
				for name, listener := range map[string]*Registry{"the first listener": first, "the second listener": second} {
					for _, instance := range []string{roleClaimSecondInstance, ""} {
						if _, err := renewRoleClaims(t, listener, holder, instance); err != nil {
							t.Fatalf("a registration by process %q through %s: %v", instance, name, err)
						}
						if stood := storedRoleClaim(t, first, role); stood != adopted {
							t.Fatalf("the claim after a registration by process %q through %s is %+v, want it exactly as the adoption left it, %+v", instance, name, stood, adopted)
						}
					}
				}
				if renewal, err := renewRoleClaims(t, first, holder, roleClaimInstance); err != nil || len(renewal.Renewed) != 1 {
					t.Fatalf("the adopting process's next registration renewed %+v, %v; want %s's claim renewed", renewal, err, role)
				}

				if _, err := first.Remove(context.Background(), holder, []string{lane}, RoleReleaseAuthority{ClientInstanceID: roleClaimSecondInstance}); err != nil {
					t.Fatalf("a second process's hand-back: %v", err)
				}
				assertRoleHolder(t, first, role, holder)
				if _, err := first.Remove(context.Background(), holder, []string{lane}, releasedByItsProcess); err != nil {
					t.Fatalf("the adopting process's own hand-back: %v", err)
				}
				assertClaimFreed(t, first, role)
			})

			t.Run("control: a claim an earlier build stored is adopted by the first registration stating a process", func(t *testing.T) {
				for name, record := range map[string]string{
					"one written before claims named a process": fmt.Sprintf(`{"holder_session_id":%q,"claimed_at":%d,"previous_session_id":""}`, holder, noProcessClaimed.UnixMilli()),
					"one an earlier build renewed":              fmt.Sprintf(`{"holder_session_id":%q,"claimed_at":%d,"previous_session_id":"","renewed_at":%d}`, holder, noProcessClaimed.UnixMilli(), noProcessClaimed.Add(beat).UnixMilli()),
				} {
					t.Run(name, func(t *testing.T) {
						first, _, adopter, advance := adoptionListeners(t, clock)
						registerSessionWithLane(t, first, holder, role)
						if _, err := first.roles().Legacy().Put(role, []byte(record)); err != nil {
							t.Fatalf("store the earlier build's record %s: %v", record, err)
						}

						advance(2 * beat)
						if _, err := renewRoleClaims(t, adopter, holder, roleClaimInstance); err != nil {
							t.Fatalf("the first registration stating a process: %v", err)
						}
						adopted := storedRoleClaim(t, first, role)
						if adopted.ClientInstanceID != roleClaimInstance || adopted.HolderSessionID != holder || adopted.ClaimedAt != noProcessClaimed.UnixMilli() {
							t.Fatalf("the claim after the first registration stating a process is %+v, want the earlier build's claim adopted by %q", adopted, roleClaimInstance)
						}
					})
				}
			})
		})
	}
}

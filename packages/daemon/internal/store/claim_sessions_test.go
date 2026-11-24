package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// claimSessionsVersion is the version of the migration that made claim_sessions, so a test can
// migrate through everything before it whatever lands beside it.
const claimSessionsVersion = 20261116144907

// sessionHold is the hold of a session id.
func sessionHold(id string) supervise.Hold {
	return supervise.Hold{Kind: supervise.HoldSessionID, Name: id}
}

// incompressible is n bytes PostgreSQL's pglz cannot shorten, so a value of it is stored, and
// indexed, at its full length: the base64 of a chain of SHA-256 digests.
func incompressible(n int) string {
	var raw []byte
	for sum := sha256.Sum256([]byte("claim_sessions")); len(raw)*4/3 < n+4; sum = sha256.Sum256(sum[:]) {
		raw = append(raw, sum[:]...)
	}
	return base64.StdEncoding.EncodeToString(raw)[:n]
}

// holdOne holds the one name h for token, and answers the claim that holds it.
func holdOne(ctx context.Context, t *testing.T, s *Store, project string, token claim.Token, h supervise.Hold) claim.Token {
	t.Helper()
	holders, err := s.HoldSessions(ctx, project, token, []supervise.Hold{h})
	if err != nil || len(holders) != 1 {
		t.Fatalf("%s holding %s %s answers %v, %v", token, h.Kind, h.Name, holders, err)
	}
	return holders[0]
}

// Each name of a session is held for the first claim of its project that records it
// (plan E's t-e-54), and nothing that happens to that claim after frees it:
// two claims racing for the same new names are answered one holder, a call records all of its
// names or none, the hold outlives the claim's retirement and its row, and a name two claims
// recorded before the table existed is held for neither and named for an operator to settle. A hold
// is its project's, as a daemon serves one project: another project's claim of the same name is
// another daemon's, whose pods the broker keeps apart (exchange.PodLaunchPrincipal).
//
// The controls (the pull request records each run): a conflict that hands the name to its latest
// caller (`on conflict (project, kind, name) do update set claim = excluded.claim`) answers the
// second claim as the holder; a call that commits the names it could take answers a third claim
// as refused a name the second never held.
func TestTheFirstClaimHoldsASessionId(t *testing.T) {
	const first, second, third = claim.Token("legion-LEGION-209-implementer"), claim.Token("legion-LEGION-209-tester"), claim.Token("legion-LEGION-209-reviewer")
	const project = "legion"

	t.Run("the first claim to record a name holds it, and its own is answered again", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		for _, ask := range []struct {
			token, want claim.Token
		}{{first, first}, {second, first}, {first, first}} {
			if holder := holdOne(ctx, t, store, project, ask.token, sessionHold("ses_new")); holder != ask.want {
				t.Fatalf("%s holding ses_new answers %q; want %s, the first claim that recorded it", ask.token, holder, ask.want)
			}
		}
		file := supervise.Hold{Kind: supervise.HoldFileName, Name: "ses_new"}
		if holder := holdOne(ctx, t, store, project, second, file); holder != second {
			t.Fatalf("%s holding a file named like first's id answers %q; want %s: each kind of name is held apart", second, holder, second)
		}
		const elsewhere = claim.Token("other-OTHER-1-implementer")
		if holder := holdOne(ctx, t, store, "other", elsewhere, sessionHold("ses_new")); holder != elsewhere {
			t.Fatalf("another project's claim holding ses_new answers %q; want %s: a hold is its own project's", holder, elsewhere)
		}
	})

	t.Run("two claims racing for the same new names are answered one holder", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		for race := range 25 {
			names := []supervise.Hold{
				sessionHold(fmt.Sprintf("ses_raced_%d", race)),
				{Kind: supervise.HoldFileName, Name: fmt.Sprintf("2026_ses_raced_%d.jsonl", race)},
				{Kind: supervise.HoldFileID, Name: fmt.Sprintf("ses_raced_%d", race)},
			}
			// The two ask for the names in opposite orders: the store takes them in one order, so
			// the two wait on each other one way and neither deadlocks.
			reversed := slices.Clone(names)
			slices.Reverse(reversed)
			var wg sync.WaitGroup
			start := make(chan struct{})
			holders := make([][]claim.Token, 2)
			errs := make([]error, 2)
			for i, ask := range []struct {
				token claim.Token
				names []supervise.Hold
			}{{first, names}, {second, reversed}} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					holders[i], errs[i] = store.HoldSessions(ctx, project, ask.token, ask.names)
				}()
			}
			close(start)
			wg.Wait()
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("race %d: the holds failed: %v, %v", race, errs[0], errs[1])
			}
			winner := holders[0][0]
			if winner != first && winner != second {
				t.Fatalf("race %d: %s was answered %s, want one of the two", race, names[0].Name, winner)
			}
			for i, h := range names {
				if holders[0][i] != winner || holders[1][len(names)-1-i] != winner {
					t.Fatalf("race %d: %s %s was answered %s and %s, want %s to both, the holder of every name",
						race, h.Kind, h.Name, holders[0][i], holders[1][len(names)-1-i], winner)
				}
			}
		}
	})

	t.Run("a call records all of its names or none", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		holdOne(ctx, t, store, project, first, sessionHold("ses_a"))
		holders, err := store.HoldSessions(ctx, project, second, []supervise.Hold{sessionHold("ses_b"), sessionHold("ses_a")})
		if err != nil || len(holders) != 2 || holders[1] != first {
			t.Fatalf("%s holding ses_b and first's ses_a answers %v, %v; want ses_a answered as %s's", second, holders, err, first)
		}
		if holder := holdOne(ctx, t, store, project, third, sessionHold("ses_b")); holder != third {
			t.Fatalf("ses_b is held for %q after a refused call named it; want it free, so %s takes it", holder, third)
		}
	})

	t.Run("a hold survives its claim's retirement and its row", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		retired := tmuxClaim(first)
		retired.Session = "ses_retired"
		if err := store.PutClaim(ctx, retired); err != nil {
			t.Fatalf("put the claim: %v", err)
		}
		if holder := holdOne(ctx, t, store, project, first, sessionHold("ses_retired")); holder != first {
			t.Fatalf("the claim holding its session answers %q", holder)
		}
		retired.State, retired.Locator, retired.CapabilityHash = supervise.StateRetired, nil, nil
		if err := store.PutClaim(ctx, retired); err != nil {
			t.Fatalf("write the retirement: %v", err)
		}
		if holder := holdOne(ctx, t, store, project, second, sessionHold("ses_retired")); holder != first {
			t.Fatalf("another claim holding the retired claim's session answers %q; want %s", holder, first)
		}
		if _, err := store.Pool().Exec(ctx, `delete from claims where token = $1`, string(first)); err != nil {
			t.Fatalf("delete the claim's row: %v", err)
		}
		if holder := holdOne(ctx, t, store, project, second, sessionHold("ses_retired")); holder != first {
			t.Fatalf("another claim holding a deleted claim's session answers %q; want %s", holder, first)
		}
	})

	// The migration's backfill is SQL, and the rule a registration holds by is Go
	// (supervise.RecordedHolds): the rows below are what each answers for the same claims, every
	// kind of name and its edges among them, and the two must be one answer. A name past
	// claim.MaxSessionNameBytes is held by neither, and does not stop the migration: a 3,200-byte id
	// would be past the 2,704 bytes a btree row holds (review N3).
	t.Run("the migration holds each recorded name as RecordedHolds does, and one two claims record for none", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, claimSessionsVersion-1)
		type recorded struct{ session, file string }
		rows := []struct {
			token, project string
			now, abandoned recorded
		}{
			{"legion-LEGION-301-implementer", "legion", recorded{"ses_own", "/state/sessions/--w--/2026-11-16T00-00-00-000Z_ses_own.jsonl"}, recorded{}},
			{"legion-LEGION-301-tester", "legion", recorded{"ses_shared", "/state/sessions/a/2026-11-16T01_ses_shared.jsonl"}, recorded{}},
			{"legion-LEGION-302-implementer", "legion", recorded{"ses_shared", "/state/sessions/b/2026-11-16T02_ses_shared.jsonl"}, recorded{}},
			{"legion-LEGION-303-implementer", "legion", recorded{}, recorded{"ses_abandoned", "/claude/projects/-w-/11111111-2222-4333-8444-555555555555.jsonl"}},
			{"legion-LEGION-304-implementer", "legion", recorded{"ses_taken", "/p/one/same.jsonl"}, recorded{}},
			{"legion-LEGION-305-implementer", "legion", recorded{}, recorded{"ses_taken", "/p/two/same.jsonl"}},
			{"legion-LEGION-306-implementer", "legion", recorded{}, recorded{}},
			{"legion-LEGION-307-implementer", "legion", recorded{"ses_307", "/sessions/w/stamp_.jsonl"}, recorded{}},
			{"legion-LEGION-308-implementer", "legion", recorded{"ses_308", "/sessions/w/a_b_c.jsonl"}, recorded{"", "/trailing/dir/"}},
			{"legion-LEGION-309-implementer", "legion", recorded{"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "/c/projects/-w-/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.jsonl"}, recorded{}},
			{"other-OTHER-301-implementer", "other", recorded{"ses_own", "/state/sessions/--w--/2026-11-16T00-00-00-000Z_ses_own.jsonl"}, recorded{}},
			{"legion-LEGION-310-implementer", "legion", recorded{incompressible(3200), "/sessions/w/" + strings.Repeat("f", 300) + ".jsonl"}, recorded{}},
			{"legion-LEGION-311-implementer", "legion", recorded{strings.Repeat("s", claim.MaxSessionNameBytes), "/sessions/w/" + strings.Repeat("f", claim.MaxSessionNameBytes-len(".jsonl")) + ".jsonl"}, recorded{}},
		}
		type key struct {
			project string
			hold    supervise.Hold
		}
		// current and abandoned are the claims recording each name in their current session and
		// only in the session they last abandoned, as the boot reads them (store.recordedNames).
		current, abandonedOnly := map[key][]claim.Token{}, map[key][]claim.Token{}
		for _, row := range rows {
			var abandoned any
			if row.abandoned != (recorded{}) {
				encoded, err := json.Marshal(map[string]string{
					"session": row.abandoned.session, "sessionFile": row.abandoned.file,
					"why": "a retry asked for a new session", "at": "2026-11-16T00:00:00Z",
				})
				if err != nil {
					t.Fatal(err)
				}
				abandoned = string(encoded)
			}
			if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
				session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
				abandoned_session)
				values ($1, $2, 'LEGION-301', 'LEGION-301', 'implementer', 1, $3, $4, 'retired', '', 0, 0, 0, 0, $5::jsonb)`,
				row.token, row.project, row.now.session, row.now.file, abandoned); err != nil {
				t.Fatalf("record %s before the migration: %v", row.token, err)
			}
			for _, h := range supervise.RecordedHolds(row.now.session, row.now.file) {
				k := key{row.project, h}
				if !slices.Contains(current[k], claim.Token(row.token)) {
					current[k] = append(current[k], claim.Token(row.token))
				}
			}
			for _, h := range supervise.RecordedHolds(row.abandoned.session, row.abandoned.file) {
				k := key{row.project, h}
				if !slices.Contains(current[k], claim.Token(row.token)) && !slices.Contains(abandonedOnly[k], claim.Token(row.token)) {
					abandonedOnly[k] = append(abandonedOnly[k], claim.Token(row.token))
				}
			}
		}
		want := map[key]claim.Token{}
		for _, recorders := range []map[key][]claim.Token{current, abandonedOnly} {
			for k := range recorders {
				if tokens := append(slices.Clone(current[k]), abandonedOnly[k]...); len(tokens) == 1 {
					want[k] = tokens[0]
				} else {
					want[k] = ""
				}
			}
		}
		for k := range want {
			if len(k.hold.Name) > claim.MaxSessionNameBytes {
				t.Errorf("RecordedHolds names %s of %d bytes, past the bound", k.hold.Kind, len(k.hold.Name))
			}
		}
		if _, ok := want[key{"legion", sessionHold(strings.Repeat("s", claim.MaxSessionNameBytes))}]; !ok {
			t.Errorf("RecordedHolds names no session id of exactly %d bytes, the bound", claim.MaxSessionNameBytes)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through the session holds: %v", err)
		}

		held := map[key]claim.Token{}
		read, err := store.Pool().Query(ctx, `select project, kind, name, claim from claim_sessions`)
		if err != nil {
			t.Fatalf("read the holds: %v", err)
		}
		for read.Next() {
			var project, kind, name, holder string
			if err := read.Scan(&project, &kind, &name, &holder); err != nil {
				t.Fatalf("scan a hold: %v", err)
			}
			held[key{project, supervise.Hold{Kind: supervise.HoldKind(kind), Name: name}}] = claim.Token(holder)
		}
		if err := read.Err(); err != nil {
			t.Fatalf("read the holds: %v", err)
		}
		for k, holder := range want {
			if got, ok := held[k]; !ok || got != holder {
				t.Errorf("the migration holds %s %s %s for %q (present %t); RecordedHolds says %q", k.project, k.hold.Kind, k.hold.Name, got, ok, holder)
			}
		}
		for k, holder := range held {
			if _, ok := want[k]; !ok {
				t.Errorf("the migration holds %s %s %s for %q, which RecordedHolds names for no claim", k.project, k.hold.Kind, k.hold.Name, holder)
			}
		}
		// The edges RecordedHolds and the SQL must agree on, named so a change to both that drops
		// one is still caught.
		for _, k := range []key{
			{"legion", supervise.Hold{Kind: supervise.HoldFileID, Name: "ses_shared"}},
			{"legion", supervise.Hold{Kind: supervise.HoldFileName, Name: "same.jsonl"}},
			{"legion", supervise.Hold{Kind: supervise.HoldFileID, Name: "b_c"}},
			{"legion", supervise.Hold{Kind: supervise.HoldFileName, Name: "dir"}},
			{"legion", sessionHold("11111111-2222-4333-8444-555555555555")},
		} {
			if _, ok := want[k]; !ok {
				t.Errorf("RecordedHolds names no %s %s, which the rows above record", k.hold.Kind, k.hold.Name)
			}
		}

		for _, token := range []claim.Token{"legion-LEGION-301-tester", "legion-LEGION-302-implementer"} {
			if holder := holdOne(ctx, t, store, "legion", token, sessionHold("ses_shared")); holder != "" {
				t.Errorf("%s holding the id two claims record answers %q; want the empty holder no claim matches", token, holder)
			}
		}

		conflicts, err := store.SessionConflicts(ctx, "legion")
		if err != nil {
			t.Fatalf("read the session conflicts: %v", err)
		}
		var wantConflicts []SessionConflict
		for k, holder := range want {
			if k.project == "legion" && holder == "" {
				c := SessionConflict{Hold: k.hold, Claims: slices.Clone(current[k]), Abandoned: slices.Clone(abandonedOnly[k])}
				slices.Sort(c.Claims)
				slices.Sort(c.Abandoned)
				wantConflicts = append(wantConflicts, c)
			}
		}
		if len(wantConflicts) != 4 {
			t.Fatalf("the rows above give %d names held for none, want 4: %+v", len(wantConflicts), wantConflicts)
		}
		// A name no row can hold, which a daemon from before the bound recorded, is named too, for
		// the claim whose current session records it (review round 11): 310's id and its file's name.
		for _, row := range rows {
			if row.project != "legion" {
				continue
			}
			for _, h := range supervise.UnholdableNames(row.now.session, row.now.file) {
				wantConflicts = append(wantConflicts, SessionConflict{Hold: h, Claims: []claim.Token{claim.Token(row.token)}, Unholdable: true})
			}
		}
		slices.SortFunc(wantConflicts, func(a, b SessionConflict) int {
			return cmp.Or(cmp.Compare(a.Hold.Kind, b.Hold.Kind), cmp.Compare(a.Hold.Name, b.Hold.Name))
		})
		if len(wantConflicts) != 6 {
			t.Fatalf("the rows above give %d conflicts, want the 4 held for none and 310's 2 no row can hold: %+v", len(wantConflicts), wantConflicts)
		}
		sameConflicts := func(a, b SessionConflict) bool {
			return a.Hold == b.Hold && a.Holder == b.Holder && slices.Equal(a.Claims, b.Claims) && slices.Equal(a.Abandoned, b.Abandoned) && a.Unholdable == b.Unholdable
		}
		if !slices.EqualFunc(conflicts, wantConflicts, sameConflicts) {
			t.Errorf("the session conflicts read %+v, want %+v", conflicts, wantConflicts)
		}
		// ses_taken: 304's current session records it and 305's abandoned one, so the boot suspends
		// 304 alone (review R2-2).
		taken := SessionConflict{Hold: sessionHold("ses_taken"), Claims: []claim.Token{"legion-LEGION-304-implementer"}, Abandoned: []claim.Token{"legion-LEGION-305-implementer"}}
		if !slices.ContainsFunc(conflicts, func(c SessionConflict) bool { return sameConflicts(c, taken) }) {
			t.Errorf("the session conflicts read %+v, want %+v among them", conflicts, taken)
		}

		// A daemon from before the hold, as one rolled back is, records names no row holds: one
		// another claim holds already, and one no row holds. The boot's backfill holds the second
		// for its claim and leaves the first as it is, and the conflict read names the claim that
		// took the first.
		if _, err := store.Pool().Exec(ctx, `update claims set session = 'ses_own', session_file = '/rolled/back/took.jsonl'
			where token = 'legion-LEGION-306-implementer'`); err != nil {
			t.Fatalf("record what a rolled-back daemon let a claim register: %v", err)
		}
		added, err := store.HoldRecordedSessions(ctx, "legion")
		if err != nil {
			t.Fatalf("hold the recorded names: %v", err)
		}
		if added != 1 {
			t.Errorf("the boot's backfill held %d names, want 1: the rolled-back claim's file name", added)
		}
		if holder := holdOne(ctx, t, store, "legion", "legion-LEGION-399-implementer", supervise.Hold{Kind: supervise.HoldFileName, Name: "took.jsonl"}); holder != "legion-LEGION-306-implementer" {
			t.Errorf("the rolled-back claim's file is held for %q after the backfill, want its own claim", holder)
		}
		if again, err := store.HoldRecordedSessions(ctx, "legion"); err != nil || again != 0 {
			t.Errorf("the backfill run again held %d names, %v; want none: every recorded name is held", again, err)
		}
		conflicts, err = store.SessionConflicts(ctx, "legion")
		if err != nil {
			t.Fatalf("read the session conflicts: %v", err)
		}
		took := SessionConflict{Hold: sessionHold("ses_own"), Holder: "legion-LEGION-301-implementer", Claims: []claim.Token{"legion-LEGION-306-implementer"}}
		if !slices.ContainsFunc(conflicts, func(c SessionConflict) bool { return sameConflicts(c, took) }) || len(conflicts) != len(wantConflicts)+1 {
			t.Errorf("the session conflicts read %+v, want the six above and %+v", conflicts, took)
		}
	})
}

// A claim holds at most supervise.MaxHeldNamesPerClaim names (review N2 of #4220). A call whose new
// names fill the cap exactly records them; one whose new names would take the claim past it answers
// supervise.ErrHeldNamesCap and records none of them, the claim keeping every row it held; a call of
// names the claim holds already adds none and is answered as before; and another claim of the
// project holds its own names as before. However many calls a claim makes, its rows stop at the cap.
//
// The control (the pull request records the run): HoldSessions with no count after its inserts;
// the calls past the cap are admitted, and the claim's rows grow past it.
func TestAClaimsHeldNamesStopAtTheCap(t *testing.T) {
	const holder, other = claim.Token("legion-LEGION-401-implementer"), claim.Token("legion-LEGION-402-implementer")
	ctx := context.Background()
	store := migratedStore(t)
	heldBy := func(token claim.Token) int {
		t.Helper()
		var n int
		if err := store.Pool().QueryRow(ctx, `select count(*) from claim_sessions where project = 'legion' and claim = $1`, string(token)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The holder registers sessions of three names each, as an Oh My Pi session gives, until the
	// next would pass the cap.
	session := func(i int) []supervise.Hold {
		return supervise.RecordedHolds(fmt.Sprintf("ses_%04d", i), fmt.Sprintf("/state/sessions/--w--/2026-11-16T00-00-00-000Z_ses_%04d.jsonl", i))
	}
	if got := len(session(0)); got != 3 {
		t.Fatalf("an Oh My Pi session gives %d names, want 3", got)
	}
	full := supervise.MaxHeldNamesPerClaim / 3
	for i := range full {
		if _, err := store.HoldSessions(ctx, "legion", holder, session(i)); err != nil {
			t.Fatalf("session %d of %d under the cap: %v", i, full, err)
		}
	}
	// Fill the cap exactly with single names.
	for i := heldBy(holder); i < supervise.MaxHeldNamesPerClaim; i++ {
		if holders, err := store.HoldSessions(ctx, "legion", holder, []supervise.Hold{sessionHold(fmt.Sprintf("ses_fill_%d", i))}); err != nil || holders[0] != holder {
			t.Fatalf("a name up to the cap answers %v, %v", holders, err)
		}
	}
	if got := heldBy(holder); got != supervise.MaxHeldNamesPerClaim {
		t.Fatalf("the claim holds %d names, want the cap, %d", got, supervise.MaxHeldNamesPerClaim)
	}

	for attempt := range 5 {
		_, err := store.HoldSessions(ctx, "legion", holder, session(full+attempt))
		if !errors.Is(err, supervise.ErrHeldNamesCap) {
			t.Errorf("a session past the cap (attempt %d) answers %v, want supervise.ErrHeldNamesCap", attempt, err)
		}
	}
	if got := heldBy(holder); got != supervise.MaxHeldNamesPerClaim {
		t.Errorf("the claim holds %d names after calls past the cap, want %d: its rows stop at the cap", got, supervise.MaxHeldNamesPerClaim)
	}
	for _, h := range session(full) {
		if holder := holdOne(ctx, t, store, "legion", other, h); holder != other {
			t.Errorf("%s %s, which the refused call named, is held for %q; want it free, so the other claim takes it", h.Kind, h.Name, holder)
		}
	}
	if holders, err := store.HoldSessions(ctx, "legion", holder, session(0)); err != nil || slices.ContainsFunc(holders, func(c claim.Token) bool { return c != holder }) {
		t.Errorf("the claim at the cap holding its own first session answers %v, %v; want it answered as its own", holders, err)
	}
}

// An operator's release frees a claim's names through ReleaseHolds (supervise.Machine.ReleaseNames),
// which deletes only the names the table holds for that claim: asked to free every name of two
// sessions, one its own and one another claim's, it frees and answers its own, in kind and then
// name order, and the other claim's stay that claim's. Once freed, another claim takes them.
// ClaimHolds answers what the claim holds, in the same order.
//
// The control (the pull request records the run): the delete without its `h.claim = $2` clause
// frees the other claim's names too, and the test fails naming them.
func TestReleaseHoldsFreesOnlyItsOwnClaimsNames(t *testing.T) {
	const holder, other = claim.Token("legion-LEGION-501-implementer"), claim.Token("legion-LEGION-502-implementer")
	ctx := context.Background()
	store := migratedStore(t)
	mineFile := "/state/sessions/--w--/2026-11-16T09-00-00-000Z_ses_mine.jsonl"
	recordAbandoningClaim(t, store, holder, "ses_current", "/state/sessions/--w--/2026-11-16T10-00-00-000Z_ses_current.jsonl", "ses_mine", mineFile, false)
	mine := supervise.RecordedHolds("ses_mine", mineFile)
	theirs := supervise.RecordedHolds("ses_theirs", "/state/sessions/--w--/2026-11-16T09-00-00-000Z_ses_theirs.jsonl")
	for token, holds := range map[claim.Token][]supervise.Hold{holder: mine, other: theirs} {
		if _, err := store.HoldSessions(ctx, "legion", token, holds); err != nil {
			t.Fatalf("hold %s's session: %v", token, err)
		}
	}
	sorted := slices.Clone(mine)
	slices.SortFunc(sorted, supervise.CompareHolds)
	if got, err := store.ClaimHolds(ctx, "legion", holder); err != nil || !slices.Equal(got, sorted) {
		t.Fatalf("ClaimHolds = %v, %v; want %v", got, err, sorted)
	}

	freed, err := store.ReleaseHolds(ctx, onlyClaim(t, store), append(slices.Clone(theirs), mine...))
	if err != nil {
		t.Fatalf("ReleaseHolds: %v", err)
	}
	if !slices.Equal(freed, sorted) {
		t.Errorf("ReleaseHolds freed %v, want only the claim's own %v", freed, sorted)
	}
	for _, h := range theirs {
		if got := holdOne(ctx, t, store, "legion", holder, h); got != other {
			t.Errorf("%s %s is held for %q after the release, want it still %s's", h.Kind, h.Name, got, other)
		}
	}
	if got, err := store.ClaimHolds(ctx, "legion", holder); err != nil || len(got) != 0 {
		t.Errorf("ClaimHolds after the release = %v, %v; want none", got, err)
	}
	for _, h := range mine {
		if got := holdOne(ctx, t, store, "legion", other, h); got != other {
			t.Errorf("%s %s, freed, is held for %q when the other claim asks; want it the other claim's", h.Kind, h.Name, got)
		}
	}
}

// A release is one transaction (ReleaseHolds): the delete of the claim's names and the write of
// the claim that marks its abandoned session released commit together or not at all (review of
// #4355). With the claim's write refused by the database, the release frees no name, and the
// claim read back after it — what the next boot reads — still records the abandoned session as
// not released, while the table still holds that session's names for it: the record and the
// table agree, and the boot has nothing to hold. With the refusal gone, the release frees the
// names and records the mark together, and the next boot holds none of them again.
//
// The control (the pull request records the run): the delete committed on the pool before the
// transaction, two commits, frees the names although the claim write was refused, and the test
// fails counting them.
func TestAReleaseWhoseClaimWriteIsRefusedFreesNoName(t *testing.T) {
	const holder = claim.Token("legion-LEGION-701-implementer")
	ctx := context.Background()
	store := migratedStore(t)
	recordAbandoningClaim(t, store, holder, "ses_current", abandoningFile("ses_current"), "ses_old", abandoningFile("ses_old"), false)
	if _, err := store.HoldRecordedSessions(ctx, "legion"); err != nil {
		t.Fatalf("HoldRecordedSessions: %v", err)
	}
	current, old := supervise.RecordedHolds("ses_current", abandoningFile("ses_current")), supervise.RecordedHolds("ses_old", abandoningFile("ses_old"))
	c := onlyClaim(t, store)
	c.Abandoned.Released = time.Now().UTC().Truncate(time.Microsecond)
	reread := func() (int, time.Time) {
		t.Helper()
		holds, err := store.ClaimHolds(ctx, "legion", holder)
		if err != nil {
			t.Fatalf("ClaimHolds: %v", err)
		}
		return len(holds), onlyClaim(t, store).Abandoned.Released
	}

	for _, statement := range []string{
		`create function refuse_claim_write() returns trigger language plpgsql as $$
			begin raise exception 'the test refuses this claim write'; end $$`,
		`create trigger refuse_claim_write before update on claims for each row
			when (old.token = '` + string(holder) + `') execute function refuse_claim_write()`,
	} {
		if _, err := store.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("refuse the claim's write: %v", err)
		}
	}
	if freed, err := store.ReleaseHolds(ctx, c, old); err == nil || !strings.Contains(err.Error(), "the test refuses this claim write") {
		t.Fatalf("the release with its claim write refused = %v, %v; want the database's refusal", freed, err)
	}
	held, released := reread()
	if held != len(current)+len(old) {
		t.Fatalf("the claim holds %d names after its release's claim write was refused, want all %d: the release freed names while the claim still records %s as not released, so the next boot would hold every one nobody took for it again",
			held, len(current)+len(old), "ses_old")
	}
	if !released.IsZero() {
		t.Fatalf("the refused claim write left the abandoned session released at %v", released)
	}
	if n, err := store.HoldRecordedSessions(ctx, "legion"); err != nil || n != 0 {
		t.Fatalf("the boot after the refused release held %d names, %v; want none: the table still holds every name the claim records", n, err)
	}

	if _, err := store.Pool().Exec(ctx, `drop trigger refuse_claim_write on claims`); err != nil {
		t.Fatalf("let the claim's write through: %v", err)
	}
	freed, err := store.ReleaseHolds(ctx, c, old)
	if err != nil || len(freed) != len(old) {
		t.Fatalf("the release once the claim write goes through = %v, %v; want the %d names of ses_old freed", freed, err, len(old))
	}
	held, released = reread()
	if held != len(current) || !released.Equal(c.Abandoned.Released) {
		t.Fatalf("after the release the claim holds %d names and marks ses_old released at %v; want %d and %v", held, released, len(current), c.Abandoned.Released)
	}
	if n, err := store.HoldRecordedSessions(ctx, "legion"); err != nil || n != 0 {
		t.Fatalf("the boot after the release held %d names, %v; want none: the released session gives none", n, err)
	}
}

// A session the claim abandoned and an operator's release then freed (supervise.AbandonedSession.
// Released) gives the boot no name: the boot holds only the names of the claim's current session,
// and once another claim takes a freed name, the boot's conflict read names no conflict for it,
// since the releasing claim records the session only to say where it went. An abandoned session no
// release freed still gives its names, as before.
//
// The control (the pull request records the run): recordedNames reading the abandoned session
// whatever its released mark holds the released session's names for the claim again at boot, and
// the test fails counting them.
func TestTheBootHoldsNoNameOfAReleasedAbandonedSession(t *testing.T) {
	const holder, other = claim.Token("legion-LEGION-601-implementer"), claim.Token("legion-LEGION-602-implementer")
	for _, tc := range []struct {
		name     string
		released bool
	}{
		{name: "released", released: true},
		{name: "not released", released: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := migratedStore(t)
			recordAbandoningClaim(t, store, holder, "ses_current", abandoningFile("ses_current"), "ses_old", abandoningFile("ses_old"), tc.released)
			held, err := store.HoldRecordedSessions(ctx, "legion")
			if err != nil {
				t.Fatalf("HoldRecordedSessions: %v", err)
			}
			current, old := supervise.RecordedHolds("ses_current", abandoningFile("ses_current")), supervise.RecordedHolds("ses_old", abandoningFile("ses_old"))
			want := len(current)
			if !tc.released {
				want += len(old)
			}
			if held != want {
				t.Fatalf("the boot held %d names, want %d: the current session's %d, and the abandoned session's %d unless it is released",
					held, want, len(current), len(old))
			}
			if !tc.released {
				return
			}
			if holders, err := store.HoldSessions(ctx, "legion", other, old); err != nil || slices.ContainsFunc(holders, func(c claim.Token) bool { return c != other }) {
				t.Fatalf("the other claim taking the released session's names = %v, %v; want them its own", holders, err)
			}
			conflicts, err := store.SessionConflicts(ctx, "legion")
			if err != nil {
				t.Fatalf("SessionConflicts: %v", err)
			}
			if len(conflicts) != 0 {
				t.Errorf("the boot reads conflicts %+v, want none: %s records the released session only to say where it went", conflicts, holder)
			}
		})
	}
}

// abandoningFile is the Oh My Pi session file of session that recordAbandoningClaim's rows name.
func abandoningFile(session string) string {
	return "/state/sessions/--w--/2026-11-16T00-00-00-000Z_" + session + ".jsonl"
}

// recordAbandoningClaim records token's claim, failed, on session and file, with the session it
// last abandoned an hour ago, its release marked when released is set.
func recordAbandoningClaim(t *testing.T, store *Store, token claim.Token, session, file, abandoned, abandonedFile string, released bool) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
		abandoned_session)
		values ($1, 'legion', 'LEGION-601', 'LEGION-601', 'implementer', 1, $2, $3, 'failed', '', 0, 0, 0, 0,
			jsonb_build_object('session', $4::text, 'sessionFile', $5::text, 'why', 'an operator gave the claim a new session',
				'at', now() - interval '1 hour')
			|| case when $6::bool then jsonb_build_object('released', now()) else '{}'::jsonb end)`,
		string(token), session, file, abandoned, abandonedFile, released); err != nil {
		t.Fatalf("record %s: %v", token, err)
	}
}

// statementTracer counts every statement a pool sends and every row its statements answer, and
// holds each statement back by delay before it goes, which gives a loopback Postgres the round trip
// of one across a network.
type statementTracer struct {
	delay time.Duration
	sent  atomic.Int64
	rows  atomic.Int64
}

func (s *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	s.sent.Add(1)
	time.Sleep(s.delay)
	return ctx
}

func (s *statementTracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.CommandTag.Select() {
		s.rows.Add(data.CommandTag.RowsAffected())
	}
}

// statementTracedStore is a second Store on store's database whose pool sends every statement
// through tracer.
func statementTracedStore(t *testing.T, store *Store, tracer pgx.QueryTracer) *Store {
	t.Helper()
	return secondStore(t, store, func(config *pgxpool.Config) { config.ConnConfig.Tracer = tracer })
}

// secondStore is a second Store on store's database, its pool configured by configure.
func secondStore(t *testing.T, store *Store, configure func(*pgxpool.Config)) *Store {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := store.Pool().QueryRow(ctx, `select current_database()`).Scan(&name); err != nil {
		t.Fatalf("read the test database's name: %v", err)
	}
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	config, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	configure(config)
	raw, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	other := &Store{pool: NewPool(raw)}
	t.Cleanup(other.Close)
	return other
}

// explainPlan is PostgreSQL's plan for query with args on store's database, as text.
func explainPlan(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), "explain (costs off) "+query, args...)
	if err != nil {
		t.Fatalf("explain the query: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the plan: %v", err)
	}
	return strings.Join(lines, "\n")
}

// tuplesReadBy is how many tuples of claim_sessions call reads, by sequential scan or by index, from
// pg_stat_user_tables. call runs on a Store of one connection, whose statistics that connection
// flushes on demand (pg_stat_force_next_flush, PostgreSQL 15 and later), so the count is its call's
// alone; nothing else reads claim_sessions while it runs. A count that never moves is no
// measurement: a conflict read of recorded names reads at least one tuple, so the test fails.
func tuplesReadBy(t *testing.T, store *Store, call func(one *Store)) int64 {
	t.Helper()
	ctx := context.Background()
	read := func() int64 {
		var tuples int64
		if _, err := store.Pool().Exec(ctx, `select pg_stat_clear_snapshot()`); err != nil {
			t.Fatal(err)
		}
		if err := store.Pool().QueryRow(ctx, `select coalesce(seq_tup_read, 0) + coalesce(idx_tup_fetch, 0)
			from pg_stat_user_tables where relname = 'claim_sessions'`).Scan(&tuples); err != nil {
			t.Fatalf("read claim_sessions' tuple counts: %v", err)
		}
		return tuples
	}
	one := secondStore(t, store, func(config *pgxpool.Config) { config.MaxConns = 1 })
	flush := func() {
		if _, err := one.Pool().Exec(ctx, `select pg_stat_force_next_flush()`); err != nil {
			t.Fatal(err)
		}
		if _, err := one.Pool().Exec(ctx, `select 1`); err != nil {
			t.Fatal(err)
		}
	}
	flush()
	before := read()
	call(one)
	flush()
	// The flush lands as the connection goes idle, so the read waits for it a little.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if after := read(); after != before {
			return after - before
		}
		if time.Now().After(deadline) {
			t.Fatalf("claim_sessions' tuple counts never moved after the conflict read: UNKNOWN, no measurement")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// productionClaims is the size of the largest project at the time of writing: its daemon booted
// with 7,508 claims on 2026-11-16 (`legion daemon started`, field claims), about 250 more a day.
const productionClaims = 7508

// The boot holds every recorded name and reads every conflict on each boot, inside a 30 s budget
// (internal/daemon's bootTimeout), so its cost must not grow with the project's names (review R2-1
// of #4220): over a project of production size, 7,508 claims recorded as Oh My Pi records them, half
// with an abandoned session, which is 33,786 names, the whole boot step is a few statements and
// takes well inside the budget while each statement is held back by a millisecond, the round trip
// of a database reached across a network. The time bound is a third of the budget, so a loaded
// runner has room; the statement bound is what does not depend on the runner.
//
// Nor may it grow with the rows the table has gathered (reviews N2 and F2): a claim may hold up to
// supervise.MaxHeldNamesPerClaim names, most of them of sessions it no longer records, and nothing
// deletes a hold. So the steady step is measured again with half a million rows no claim records
// any more, as 245 claims at the cap would hold: the conflict read must read, on the server, at
// most twice the tuples of claim_sessions that the claims' names are. The read looks each name up
// by key; the planner's choice is logged, not asserted, since it follows the table's statistics.
//
// The controls (the pull request records each run): HoldRecordedSessions as a statement per name,
// which sends 33,786 inserts and, at a millisecond each, takes past the bound; and SessionConflicts
// as 723aa7826a's join, which the planner runs over this table as a hash join over a sequential
// scan, reading every stale row.
func TestTheBootHoldsAProjectOfProductionSizeInAFewStatements(t *testing.T) {
	const (
		statementBound = 10
		timeBound      = 10 * time.Second
		names          = productionClaims*3 + productionClaims/2*3
	)
	ctx := context.Background()
	store := migratedStore(t)
	if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
		abandoned_session)
		select 'legion-LEGION-' || n || '-implementer', 'legion', 'LEGION-' || n, 'LEGION-' || n, 'implementer', 1,
			s, '/state/sessions/--w--/2026-11-16T00-00-00-000Z_' || s || '.jsonl', 'retired', '', 0, 0, 0, 0,
			case when n % 2 = 0 then jsonb_build_object('session', a,
				'sessionFile', '/state/sessions/--w--/2026-11-15T00-00-00-000Z_' || a || '.jsonl',
				'why', 'a retry asked for a new session', 'at', '2026-11-16T00:00:00Z') end
		from (select n, gen_random_uuid()::text as s, gen_random_uuid()::text as a
			from generate_series(1, $1::int) as n) as seeded`, productionClaims); err != nil {
		t.Fatalf("record %d claims: %v", productionClaims, err)
	}
	tracer := &statementTracer{delay: time.Millisecond}
	traced := statementTracedStore(t, store, tracer)

	// The first boot after a rollback holds every name.
	started := time.Now()
	held, err := traced.HoldRecordedSessions(ctx, "legion")
	firstTook, firstSent := time.Since(started), tracer.sent.Swap(0)
	if err != nil || held != names {
		t.Fatalf("the first hold held %d names, %v; want %d", held, err, names)
	}

	// Every boot after it holds nothing and reads every conflict.
	started = time.Now()
	again, err := traced.HoldRecordedSessions(ctx, "legion")
	if err != nil || again != 0 {
		t.Fatalf("the steady hold held %d names, %v; want none", again, err)
	}
	conflicts, err := traced.SessionConflicts(ctx, "legion")
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("the conflict read answered %+v, %v; want none", conflicts, err)
	}
	steadyTook, steadySent := time.Since(started), tracer.sent.Load()
	t.Logf("%d claims, %d names, a millisecond a statement: the first hold %d statements in %v; a steady boot step %d statements in %v",
		productionClaims, names, firstSent, firstTook, steadySent, steadyTook)
	// The same steady step on the pool with no added delay: on a loopback it is the step's own cost,
	// and against a database reached through a proxy that adds latency, the cost at that latency.
	started = time.Now()
	if _, err := store.HoldRecordedSessions(ctx, "legion"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionConflicts(ctx, "legion"); err != nil {
		t.Fatal(err)
	}
	t.Logf("a steady boot step on the database as LEGION_TEST_PG_DSN reaches it, with no added delay: %v", time.Since(started))

	// Half a million rows of sessions no claim records any more.
	const stale = 500_000
	if _, err := store.Pool().Exec(ctx, `insert into claim_sessions (project, kind, name, claim)
		select 'legion', 'session', 'stale_' || n, 'legion-LEGION-' || (n % $2::int + 1) || '-implementer'
		from generate_series(1, $1::int) as n`, stale, productionClaims); err != nil {
		t.Fatalf("record %d stale holds: %v", stale, err)
	}
	if _, err := store.Pool().Exec(ctx, `analyze claim_sessions`); err != nil {
		t.Fatal(err)
	}
	tracer.sent.Store(0)
	tracer.rows.Store(0)
	started = time.Now()
	if again, err := traced.HoldRecordedSessions(ctx, "legion"); err != nil || again != 0 {
		t.Fatalf("the steady hold over the stale rows held %d names, %v; want none", again, err)
	}
	if conflicts, err := traced.SessionConflicts(ctx, "legion"); err != nil || len(conflicts) != 0 {
		t.Fatalf("the conflict read over the stale rows answered %+v, %v; want none", conflicts, err)
	}
	staleTook, staleSent, staleRows := time.Since(started), tracer.sent.Load(), tracer.rows.Load()
	t.Logf("with %d more rows no claim records: a steady boot step %d statements, %d rows returned, in %v", stale, staleSent, staleRows, staleTook)
	if staleSent > statementBound || staleTook > timeBound {
		t.Errorf("with %d stale rows the steady boot step sent %d statements in %v; want at most %d statements, within %v",
			stale, staleSent, staleTook, statementBound, timeBound)
	}

	// The rows returned say nothing of the rows the server reads (review F2), so the read's tuples
	// of claim_sessions are counted too: at most twice the names the claims record. The planner's
	// choice is not asserted, since it follows the table's statistics; the plan is logged.
	var kinds, recordedNamesOf []string
	if err := Transact(ctx, store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		current, abandoned, _, err := recordedNames(ctx, tx, "legion")
		for _, h := range sortedHolds(current, abandoned) {
			kinds, recordedNamesOf = append(kinds, string(h.Kind)), append(recordedNamesOf, h.Name)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("the conflict read's plan over %d recorded names and %d rows:\n%s", len(kinds), names+stale,
		explainPlan(t, store, conflictHoldersQuery, "legion", kinds, recordedNamesOf))
	tuples := tuplesReadBy(t, store, func(one *Store) {
		if conflicts, err := one.SessionConflicts(ctx, "legion"); err != nil || len(conflicts) != 0 {
			t.Fatalf("the counted conflict read answered %+v, %v; want none", conflicts, err)
		}
	})
	t.Logf("one conflict read over %d rows read %d tuples of claim_sessions", names+stale, tuples)
	if bound := int64(2 * names); tuples > bound {
		t.Errorf("one conflict read over %d rows read %d tuples of claim_sessions; want at most %d, twice the names the claims record: its cost follows those names, not the holds the table has gathered",
			names+stale, tuples, bound)
	}

	if firstSent > statementBound || steadySent > statementBound {
		t.Errorf("the boot step sent %d statements at the first hold and %d at a steady boot; want at most %d each, whatever the project's size",
			firstSent, steadySent, statementBound)
	}
	if firstTook > timeBound || steadyTook > timeBound {
		t.Errorf("the boot step took %v at the first hold and %v at a steady boot; want each within %v, a third of the 30 s boot budget",
			firstTook, steadyTook, timeBound)
	}
}

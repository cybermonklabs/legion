//go:build !race

package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The sender's claim model (TestTheMentionSenderClaimModel) restates four claim rules at the
// slot: a claim free to take (cmFree, model.DeliveryClaimFreeSQL), a row's claim the sender owns
// by the claim it recorded (cmOwns, model.DeliveryClaimSinceSQL), a claim the refresh writes
// again (cmRefreshWrites, model.DeliveryClaimLiveSQL in refreshMentionClaims), and an attempt the
// delivery sweep takes (owed, model.OwedMentionAttemptSQL). This reads each rule and the SQL it
// restates on the same claims: a row's claim null or written a quarter or three quarters into a
// slot, the claim the sender recorded and the attempt's creation likewise, all read a quarter or
// three quarters into the slot they are read in, the SQL against Postgres's own now() with each
// claim its exact age before it.
//
// The model's rules are the SQL's widened to the slot, the way its search relies on: cmFree,
// cmOwns and cmRefreshWrites hold wherever the SQL's do, so a send the model finds refused, and a
// claim its refresh lets lapse, is so at every timing within the slots; owed holds only where the
// SQL's does, so an attempt the model's sweep takes, the SQL's takes at every timing. Where a rule
// and its SQL differ, the exact age they compare is within a slot of the lease, which the slot
// cannot tell apart; and each rule is true and false on some claims.
func TestTheClaimModelsRulesAreTheSQLsRules(t *testing.T) {
	t.Parallel()
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	ctx := context.Background()
	database := storetest.Open(t)
	type moment struct{ slot, quarter int }
	at := func(m moment) time.Time {
		return cmBase.Add(time.Duration(m.slot)*cmSlot + time.Duration(m.quarter)*cmSlot/4)
	}
	var moments []moment
	for slot := range 2*cmLapseSlots + 2 {
		moments = append(moments, moment{slot, 1}, moment{slot, 3})
	}
	lease := model.DeliveryClaimLapse
	nearLease := func(age time.Duration) bool { return age > lease-cmSlot && age < lease+cmSlot }
	// tally is one rule's agreements and disagreements with its SQL, and whether it was ever true
	// and ever false.
	type tally struct {
		name                string
		agree, widened      int
		everTrue, everFalse bool
	}
	check := func(tl *tally, modelSays, sqlSays bool, widens bool, ages []time.Duration, what string) {
		tl.everTrue = tl.everTrue || modelSays
		tl.everFalse = tl.everFalse || !modelSays
		switch {
		case modelSays == sqlSays:
			tl.agree++
			return
		case widens && modelSays || !widens && !modelSays:
			near := false
			for _, age := range ages {
				near = near || nearLease(age)
			}
			if near {
				tl.widened++
				return
			}
		}
		t.Errorf("%s: the model says %t and the SQL %t for %s, which is no slot's widening of the lease", tl.name,
			modelSays, sqlSays, what)
	}
	seconds := func(d time.Duration) float64 { return d.Seconds() }

	// Free, and owed: a row's claim (or none) and the attempt's creation, read now.
	var claimAges, createdAges []float64
	type freeCase struct {
		now, created moment
		claim        *moment
	}
	var frees []freeCase
	for _, now := range moments {
		for _, created := range moments {
			if at(created).After(at(now)) {
				continue
			}
			frees = append(frees, freeCase{now: now, created: created})
			claimAges = append(claimAges, -1)
			createdAges = append(createdAges, seconds(at(now).Sub(at(created))))
			for _, claim := range moments {
				if at(claim).Before(at(created)) || at(claim).After(at(now)) {
					continue
				}
				frees = append(frees, freeCase{now: now, created: created, claim: &claim})
				claimAges = append(claimAges, seconds(at(now).Sub(at(claim))))
				createdAges = append(createdAges, seconds(at(now).Sub(at(created))))
			}
		}
	}
	rows, err := database.Pool.Query(ctx, `
		select (select `+model.DeliveryClaimFreeSQL+` from (select x.claimed_at) c),
		       (select `+model.OwedMentionAttemptSQL("d")+` from (select gen_random_uuid() as comment_id,
		               'session:s1'::text as target, 1 as attempt, x.created_at, x.claimed_at) d)
		from (select i, case when ca < 0 then null else now() - make_interval(secs => ca) end as claimed_at,
		             now() - make_interval(secs => cr) as created_at
		      from unnest($1::float8[], $2::float8[]) with ordinality as u(ca, cr, i)) x
		order by x.i`, claimAges, createdAges)
	if err != nil {
		t.Fatalf("read the free and owed rules: %v", err)
	}
	free, owed := &tally{name: "cmFree"}, &tally{name: "owed"}
	i := 0
	for rows.Next() {
		var sqlFree, sqlOwed bool
		if err := rows.Scan(&sqlFree, &sqlOwed); err != nil {
			t.Fatalf("scan the free and owed rules: %v", err)
		}
		c := frees[i]
		r := &cmRun{slot: c.now.slot}
		row := cmRow{committed: true, pending: true, created: at(c.created)}
		ages := []time.Duration{at(c.now).Sub(at(c.created))}
		what := fmt.Sprintf("no claim, created %v before now", ages[0])
		if c.claim != nil {
			row.claim = at(*c.claim)
			ages = append(ages, at(c.now).Sub(row.claim))
			what = fmt.Sprintf("a claim %v old, created %v before now", ages[1], ages[0])
		}
		r.rows[0] = row
		check(free, r.cmFree(row.claim), sqlFree, true, ages[1:], what)
		check(owed, r.owed(0), sqlOwed, false, ages, what)
		i++
	}
	if err := rows.Err(); err != nil || i != len(frees) {
		t.Fatalf("read %d of %d free and owed cases: %v", i, len(frees), err)
	}

	// Owns: a row's claim and the claim the sender recorded; live: the claim the refresh names,
	// read now.
	type pair struct{ row, named moment }
	var pairs []pair
	var rowAges, namedAges []float64
	reference := moment{2*cmLapseSlots + 2, 3}
	for _, named := range moments {
		for _, row := range moments {
			pairs = append(pairs, pair{row: row, named: named})
			rowAges = append(rowAges, seconds(at(reference).Sub(at(row))))
			namedAges = append(namedAges, seconds(at(reference).Sub(at(named))))
		}
	}
	rows, err = database.Pool.Query(ctx, `
		select coalesce(`+model.DeliveryClaimSinceSQL("x.named")+`, false)
		from (select i, now() - make_interval(secs => ra) as claimed_at, now() - make_interval(secs => na) as named
		      from unnest($1::float8[], $2::float8[]) with ordinality as u(ra, na, i)) x
		order by x.i`, rowAges, namedAges)
	if err != nil {
		t.Fatalf("read the ownership rule: %v", err)
	}
	owns := &tally{name: "cmOwns"}
	i = 0
	for rows.Next() {
		var sqlOwns bool
		if err := rows.Scan(&sqlOwns); err != nil {
			t.Fatalf("scan the ownership rule: %v", err)
		}
		p := pairs[i]
		since := at(p.row).Sub(at(p.named))
		check(owns, cmOwns(at(p.row), at(p.named)), sqlOwns, true, []time.Duration{since},
			fmt.Sprintf("a row's claim %v after the claim recorded", since))
		i++
	}
	if err := rows.Err(); err != nil || i != len(pairs) {
		t.Fatalf("read %d of %d ownership cases: %v", i, len(pairs), err)
	}

	type liveCase struct{ now, named moment }
	var lives []liveCase
	var liveAges []float64
	for _, now := range moments {
		for _, named := range moments {
			if at(named).After(at(now)) {
				continue
			}
			lives = append(lives, liveCase{now: now, named: named})
			liveAges = append(liveAges, seconds(at(now).Sub(at(named))))
		}
	}
	rows, err = database.Pool.Query(ctx, `
		select `+model.DeliveryClaimLiveSQL("x.named")+`
		from (select i, now() - make_interval(secs => na) as named
		      from unnest($1::float8[]) with ordinality as u(na, i)) x
		order by x.i`, liveAges)
	if err != nil {
		t.Fatalf("read the refresh's rule: %v", err)
	}
	live := &tally{name: "cmRefreshWrites"}
	i = 0
	for rows.Next() {
		var sqlLive bool
		if err := rows.Scan(&sqlLive); err != nil {
			t.Fatalf("scan the refresh's rule: %v", err)
		}
		c := lives[i]
		r := &cmRun{slot: c.now.slot}
		named := at(c.named)
		age := at(c.now).Sub(named)
		check(live, r.cmRefreshWrites(cmRow{pending: true, claim: named}, named), sqlLive, true,
			[]time.Duration{age}, fmt.Sprintf("a named claim %v old", age))
		i++
	}
	if err := rows.Err(); err != nil || i != len(lives) {
		t.Fatalf("read %d of %d refresh cases: %v", i, len(lives), err)
	}

	for _, tl := range []*tally{free, owed, owns, live} {
		t.Logf("%s: %d cases agree with the SQL, %d differ within a slot of the lease", tl.name, tl.agree, tl.widened)
		if !tl.everTrue || !tl.everFalse {
			t.Errorf("%s is never %t on these cases: the test cannot tell it from a constant", tl.name, !tl.everTrue)
		}
	}
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}

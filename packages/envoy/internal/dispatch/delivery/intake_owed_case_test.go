package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The reads owed in one repository, which a read-through takes before its list read
// (owedReadsIn), are the rows owed under each name whose canonical name is the repository's: the
// name itself in any letter case and each name renamed to it (refs.NamesSQL), over the pairs the
// boot read. oldco/widgets is renamed to acme/widgets; #1 is owed under acme/widgets, #2 under
// Acme/Widgets, #3 under oldco/widgets and #4 under other/widgets. A read of acme/widgets takes
// #1, #2 and #3. A read of oldco/widgets takes none: no repository is called that today, so
// nothing GitHub answers for it settles a row. Where the names were the given name and the names
// renamed to it, with no test of the given name's own canonical name, the read of oldco/widgets
// took #3.
func TestTheReadsOwedInARepositoryAreTheRowsOfItsNames(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliveryProject(t, ctx, pool)
	renameOldcoWidgets(t, ctx, pool)
	attributor := NewAttributor(renamesOf(t, ctx, pool))
	for number, repo := range map[int]string{1: "acme/widgets", 2: "Acme/Widgets", 3: "oldco/widgets", 4: "other/widgets"} {
		if _, err := pool.Exec(ctx, `insert into pull_request_reads_owed (repo, number, reason, revision)
			values ($1, $2, 'failure', nextval('pull_request_reads_owed_revision_seq'))`, repo, number); err != nil {
			t.Fatalf("owe the read of %s#%d: %v", repo, number, err)
		}
	}
	read := func(repo string) string {
		t.Helper()
		owed := attributor.owedReadsIn(ctx, pool, repo)
		var listed []string
		for number, rows := range owed {
			for _, row := range rows {
				listed = append(listed, fmt.Sprintf("%s#%d", row.Repo, number))
			}
		}
		slices.Sort(listed)
		return fmt.Sprint(listed)
	}
	if got, want := read("acme/widgets"), "[Acme/Widgets#2 acme/widgets#1 oldco/widgets#3]"; got != want {
		t.Errorf("the reads owed in acme/widgets are %s, want %s: its name in any case and the name renamed to it", got, want)
	}
	if got := read("oldco/widgets"); got != "[]" {
		t.Errorf("the reads owed in oldco/widgets are %s, want none: it is renamed away, so it names no repository GitHub answers for", got)
	}
}

// A read the intake makes settles the read a link owed in another letter case. A link by hand owes
// the read of #4297 in the link's own letters, acme/widgets, while no head row of it is stored.
// GitHub's event of the same pull request names the repository Acme/Widgets, and the intake's read
// of it answers. The read settles the link's row (owedRevision finds it in any case, and
// settleOwedRead deletes it by its own name), so no read of #4297 stays owed and the reconcile
// reads it no more. Where the intake read the owed revision in exact case, it found no row,
// settled nothing, and the reconcile read the pull request once more. The control: a link written
// in GitHub's own letters is settled the same way.
func TestTheIntakesReadSettlesTheReadALinkOwedInAnotherCase(t *testing.T) {
	for _, linked := range []string{"acme/widgets", "Acme/Widgets"} {
		t.Run(linked, func(t *testing.T) {
			fastPageRetries(t)
			pool, ctx := deliveryTestPool(t)
			const number = 4297
			const head, branch = "4297429742974297429742974297429742974297", "feature/case"
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := unrenamed().OweLinkedPullRequestReads(ctx, tx, []string{"https://github.com/" + linked + "/pull/" + strconv.Itoa(number)}); err != nil {
				t.Fatalf("owe the read of the link to %s#%d: %v", linked, number, err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			owedInAnyCase := func() int {
				t.Helper()
				var owed int
				if err := pool.QueryRow(ctx, `select count(*) from pull_request_reads_owed where lower(repo) = 'acme/widgets' and number = $1`, number).Scan(&owed); err != nil {
					t.Fatalf("count the reads of #%d owed: %v", number, err)
				}
				return owed
			}
			if owed := owedInAnyCase(); owed != 1 || owedCount(t, ctx, pool, linked, number) != 1 {
				t.Fatalf("after the link to %s#%d, %d reads of it are owed in any case; want the one row under %s", linked, number, owed, linked)
			}
			fake := newFakeGitHub(t)
			github := serveGitHubPulls(t, fake, "Acme/Widgets")
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			github.set("Acme/Widgets", number, "open", head, branch, "", at)
			intake := newIntake(tuningFor(t), nil, pool, nil, fake.newTestClient(), nil, newSettingsCache(pool, nil, tuningFor(t).log), unrenamed())
			payload, err := json.Marshal(prEnvelope("Acme/Widgets", number, "synchronize", head, branch, "", at))
			if err != nil {
				t.Fatal(err)
			}
			intake.deliver(deliveryWithNoNATS(ctx), intakeMessage{
				msg:      &natsgo.Msg{Subject: "notifications.github.Acme.Widgets.pr." + strconv.Itoa(number)},
				envelope: &contracts.Envelope{Payload: string(payload)},
			})
			var heads int
			if err := pool.QueryRow(ctx, `select count(*) from pull_request_heads where lower(repo) = 'acme/widgets' and number = $1 and head_sha = $2`, number, head).Scan(&heads); err != nil {
				t.Fatal(err)
			}
			if heads != 1 {
				t.Fatalf("after the event's delivery, %d head rows of #%d hold %s; want 1: the intake's read answered", heads, number, head)
			}
			if owed := owedInAnyCase(); owed != 0 {
				t.Errorf("after the intake's read of Acme/Widgets#%d answered, %d reads of it owed under %s stay; want none: the read settles the row the link owed, in any case", number, owed, linked)
			}
		})
	}
}

// deliveryWithNoNATS is the delivery a test hands Intake.deliver to drive one envelope through the
// intake with no NATS behind it. The test's message is bound to no subscription, so its
// acknowledgement fails and the answer path hands the message back (acknowledge); the delivery is
// sealed, so that hand-back is withheld (admitHandBack) rather than marked on a bus connection the
// test does not have. What the test reads is what the handling wrote, which the answer to NATS
// does not change. Its calls to NATS wait by production's tuning (callNATS), as the intake's own
// deliveries would.
func deliveryWithNoNATS(handling context.Context) *intakeDelivery {
	return &intakeDelivery{handling: handling, sealed: true, tuning: newIntakeTuning()}
}

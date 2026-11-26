package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// caughtUpState is the state of a server whose consumers have caught up as of today, at build
// "build": no Legion project and no keyword fill to wait for, the refresher's cursors at the heads,
// the delivery walk done and INFRA's stored figures walk done and built through the last end day,
// and every family served by the build.
func caughtUpState(registry bool) (consumerState, *heads) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	at := heads{events: 10, observations: 5, changes: 20}
	return consumerState{
		heads:    at,
		registry: registry,
		days:     map[string]int64{"events": 10, "delivery_changes": 20, "figures": 20},
		walks: []walk{
			{family: "delivery", project: "", next: new(walkDone("delivery", today))},
			{family: "figures", project: "INFRA", next: new(walkDone("figures", today))},
		},
		serving:     map[string]string{"delivery/": "build"},
		builtBy:     map[string]int64{"build": 12},
		dayProjects: []string{"INFRA"},
		built:       map[string]time.Time{"INFRA": today.AddDate(0, 0, evals.FigureLead)},
	}, &at
}

// TestTheWaitHoldsUntilTheRegroupHasBuiltTheProjectsCodeDayRows: where the database holds the code
// survival results, the consumers have not caught up while the regroup's pass at the server's
// start has stored no digest for the measured project, whatever the refresher has done, and have
// once it has; the rows of another project are no condition of it; and a database that holds no
// code_projects asks nothing of the regroup.
func TestTheWaitHoldsUntilTheRegroupHasBuiltTheProjectsCodeDayRows(t *testing.T) {
	t.Parallel()
	state, previous := caughtUpState(true)
	if missing := state.caughtUp("build", previous, "INFRA"); len(missing) != 0 {
		t.Fatalf("a state with no code_projects owes %q, want nothing", missing)
	}
	state.codeBuilt = map[string]bool{"ALPHA": true}
	missing := state.caughtUp("build", previous, "INFRA")
	if len(missing) != 1 || !strings.Contains(missing[0], "regroup") || !strings.Contains(missing[0], "INFRA") {
		t.Errorf("a state whose regroup built ALPHA alone owes %q, want one line naming the regroup of INFRA", missing)
	}
	state.codeBuilt["INFRA"] = true
	if missing := state.caughtUp("build", previous, "INFRA"); len(missing) != 0 {
		t.Errorf("a state whose regroup built INFRA owes %q, want nothing", missing)
	}
	if line := state.summary("build"); !strings.Contains(line, "code_projects_built=2") {
		t.Errorf("the summary %q does not count the projects the regroup built", line)
	}
}

// TestABaseThatHoldsNoRefresherIsOwedNoDayRowsAndOneThatHoldsItIsOwedThem: the base before the eval
// registry runs no refresher, so only the facts and the fill are waited for; a base that is a commit
// of this branch before the code evals runs it, so its walks, its serving records and its stored
// figures are waited for as this head's are, and the regroup, which it does not have, is not.
func TestABaseThatHoldsNoRefresherIsOwedNoDayRowsAndOneThatHoldsItIsOwedThem(t *testing.T) {
	t.Parallel()
	bare := consumerState{heads: heads{events: 10}, registry: false}
	if missing := bare.caughtUp("build", &heads{events: 10}, "INFRA"); len(missing) != 0 {
		t.Errorf("a base with no refresher owes %q, want nothing", missing)
	}
	state, previous := caughtUpState(true)
	state.walks = state.walks[:1]
	missing := state.caughtUp("build", previous, "INFRA")
	if len(missing) == 0 || !strings.Contains(strings.Join(missing, "\n"), "figures/INFRA") {
		t.Errorf("a base whose refresher has not walked INFRA's figures owes %q, want the walk named", missing)
	}
}

// TestTheFirstPollsOweTheHeadsTheCursorsAreHeldTo: with no earlier heads to hold the cursors to, the
// consumers are not caught up, the regroup's condition or none.
func TestTheFirstPollsOweTheHeadsTheCursorsAreHeldTo(t *testing.T) {
	t.Parallel()
	state, _ := caughtUpState(true)
	state.codeBuilt = map[string]bool{"INFRA": true}
	if missing := state.caughtUp("build", nil, "INFRA"); len(missing) != 1 || !strings.Contains(missing[0], "polls") {
		t.Errorf("the first poll owes %q, want the polls the heads need", missing)
	}
}

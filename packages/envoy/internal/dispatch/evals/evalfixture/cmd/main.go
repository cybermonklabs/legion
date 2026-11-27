// Command evalfixture loads the histories the eval registry's checks read (dispatch://INFRA-2916,
// k6, k12 and k15, the base whose first paint k8's and k13's negative controls read, and
// dispatch://INFRA-978's) into a database that already holds Dispatch's schema, and prints, as
// JSON, what each mode wrote: the project, the counts, the times a check reads its windows from,
// for -attribution each row with the projects it counts for, for -failures every deploy and
// failure row with the counts each check expects, and the seed where -delivery, the one seeded
// mode, ran.
//
//	go run ./internal/dispatch/evals/evalfixture/cmd -database <url> -project LEGK \
//	  -legion -issues 240 -now 2026-11-13T12:00:00Z
//	go run ./internal/dispatch/evals/evalfixture/cmd -database <url> -project DELV \
//	  -delivery -days 28 -seed 1 -now 2026-11-13T12:00:00Z
//	go run ./internal/dispatch/evals/evalfixture/cmd -database <url> \
//	  -merge-pr acme/api#12 -now 2026-11-13T13:00:00Z
//	go run ./internal/dispatch/evals/evalfixture/cmd -database <url> -project FAIL \
//	  -failures -controller-session ses_controller -now 2026-11-13T12:00:00Z
//
// -legion adds to one project and writes its projects row only where it has none, so it combines
// with the other modes but -base, which writes the same history alone, into a database of the
// base. -delivery adds to one project too, beside deliverytest's second project; -attribution
// writes three projects, -project, -project+"B" and -project+"C"; and each of the two saves the
// project's delivery settings. After them each project's next issue number is moved past every issue
// it holds. Every mode but -merge-pr writes into -project, which it requires; -merge-pr names its
// pull request by repository and number and writes no project's rows, so it needs none:
//
//	-legion       legiontest's Legion history, at least -issues finished issues, observed from
//	              partway through, and the project's legion_settings row (this head's schema)
//	-base         the same history into a database of the base, main at the commit this head
//	              merges (the base evalbench measures); alone
//	-attribution  k15's case under -project, -project+"B" and -project+"C"; not with -delivery,
//	              since both write the project's delivery settings
//	-delivery     -days days of deliverytest's delivery history, seeded by -seed
//	-merge-pr     <owner>/<repo>#<n>: that pull request merged at -now (k12's one change)
//	-failures     INFRA-978's record of deploys and failed changes under -project and
//	              -project+"B", its rows written through the writers production uses (the
//	              production signals' loop among them); with -controller-session <id>, a Legion
//	              read of each project naming that session its controller (k8); not with
//	              -attribution or -delivery, which write the project's delivery settings too
//
// Every time is relative to -now, which defaults to this second: two loads with one -now hold
// the same history at the same times, and a second -failures load writes the same rows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// options is one invocation's flags.
type options struct {
	database, project, mergePR string
	legion, base, attribution  bool
	delivery, failures         bool
	controllerSession          string
	issues, days               int
	seed                       uint64
	now                        time.Time
}

// output is what one invocation wrote, by mode; a mode not asked for is left out, and so are the
// project where none was named (-merge-pr alone) and the seed where no seeded mode (-delivery)
// ran.
type output struct {
	Project     string                         `json:"project,omitempty"`
	Now         time.Time                      `json:"now"`
	Seed        *uint64                        `json:"seed,omitempty"`
	Legion      *evalfixture.LegionResult      `json:"legion,omitempty"`
	Base        *evalfixture.LegionResult      `json:"base,omitempty"`
	Attribution *evalfixture.AttributionResult `json:"attribution,omitempty"`
	Delivery    *evalfixture.DeliveryResult    `json:"delivery,omitempty"`
	MergePR     *evalfixture.MergeResult       `json:"merge_pr,omitempty"`
	Failures    *evalfixture.FailuresResult    `json:"failures,omitempty"`
}

func main() {
	var o options
	var now string
	flag.StringVar(&o.database, "database", "", "the Postgres URL to write into; it must hold Dispatch's schema")
	flag.StringVar(&o.project, "project", "", "the project every mode writes into; required by every mode but -merge-pr, which writes no project's rows")
	flag.BoolVar(&o.legion, "legion", false, "write the Legion history and the project's legion_settings row")
	flag.BoolVar(&o.base, "base", false, "write the Legion history into a database of the base: main at the commit this head merges (git merge-base <head> origin/main)")
	flag.BoolVar(&o.attribution, "attribution", false, "write k15's attribution case")
	flag.BoolVar(&o.delivery, "delivery", false, "write -days days of delivery rows for the project")
	flag.StringVar(&o.mergePR, "merge-pr", "", "merge the pull request <owner>/<repo>#<n> at -now")
	flag.BoolVar(&o.failures, "failures", false, "write INFRA-978's record of deploys and failed changes under -project and -project+\"B\"")
	flag.StringVar(&o.controllerSession, "controller-session", "", "with -failures, record a Legion read of each project naming this session its controller")
	flag.IntVar(&o.issues, "issues", evalfixture.DefaultIssues, "how many finished issues the Legion history holds at least")
	flag.IntVar(&o.days, "days", 7, "how many days of delivery rows -delivery writes")
	flag.Uint64Var(&o.seed, "seed", 1, "the seed of the delivery history's random draws (-delivery)")
	flag.StringVar(&now, "now", "", "the moment every history ends at, RFC 3339; this second by default")
	flag.Parse()
	o.now = time.Now().UTC().Truncate(time.Second)
	if now != "" {
		at, err := time.Parse(time.RFC3339, now)
		if err != nil {
			fail(fmt.Errorf("read -now %q as a moment: %w", now, err))
		}
		o.now = at.UTC()
	}
	if err := run(context.Background(), o, os.Stdout); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "evalfixture: %v\n", err)
	os.Exit(1)
}

// run checks the flags, writes each mode asked for and prints to out what it wrote.
func run(ctx context.Context, o options, out io.Writer) error {
	if err := check(o); err != nil {
		return err
	}
	database, err := store.Open(ctx, o.database)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer database.Pool.Close()
	pool := database.Pool
	written := output{Project: o.project, Now: o.now}
	if o.delivery {
		written.Seed = &o.seed
	}
	var projects []string
	if o.project != "" {
		projects = []string{o.project}
	}
	// -failures writes its issues first, under the projects' first numbers, so a mode that numbers
	// its issues after those a project holds (-legion) numbers after them.
	if o.failures {
		result, err := evalfixture.WriteFailures(ctx, pool, o.project, o.now, o.controllerSession)
		if err != nil {
			return err
		}
		written.Failures, projects = &result, result.Projects
	}
	if o.attribution {
		result, err := evalfixture.WriteAttribution(ctx, pool, o.project, o.now)
		if err != nil {
			return err
		}
		written.Attribution, projects = &result, result.Projects
	}
	if o.legion {
		result, err := evalfixture.WriteLegion(ctx, pool, o.project, o.issues, o.now)
		if err != nil {
			return err
		}
		written.Legion = &result
	}
	if o.base {
		result, err := evalfixture.WriteBase(ctx, pool, o.project, o.issues, o.now)
		if err != nil {
			return err
		}
		written.Base = &result
	}
	if o.delivery {
		result, err := evalfixture.WriteDelivery(ctx, pool, o.project, o.days, o.seed, o.now)
		if err != nil {
			return err
		}
		written.Delivery = &result
	}
	if o.mergePR != "" {
		result, err := evalfixture.MergePR(ctx, pool, o.mergePR, o.now)
		if err != nil {
			return err
		}
		written.MergePR = &result
	}
	// -merge-pr alone names no project and writes no issue, so it leaves every number as it is.
	if len(projects) > 0 {
		if err := evalfixture.SettleNumbers(ctx, pool, projects); err != nil {
			return err
		}
	}
	report, err := json.MarshalIndent(written, "", "  ")
	if err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	fmt.Fprintf(out, "%s\n", report)
	return nil
}

// check refuses flags that name no mode, a mode with no project it writes into, or modes that
// cannot run together.
func check(o options) error {
	switch {
	case o.project == "" && (o.legion || o.base || o.attribution || o.delivery || o.failures):
		return errors.New("name the project -legion, -base, -attribution, -delivery and -failures write into with -project; -merge-pr alone needs none")
	case o.database == "":
		return errors.New("name the database to write with -database <url>")
	case !o.legion && !o.base && !o.attribution && !o.delivery && !o.failures && o.mergePR == "":
		return errors.New("name at least one mode: -legion, -base, -attribution, -delivery, -failures or -merge-pr")
	case o.base && (o.legion || o.attribution || o.delivery || o.failures || o.mergePR != ""):
		return errors.New("-base writes the Legion history alone, into a database of the base")
	case o.attribution && o.delivery:
		return errors.New("-attribution and -delivery each write the project's delivery settings: load them into two databases")
	case o.failures && (o.attribution || o.delivery):
		return errors.New("-failures writes the project's delivery settings, as -attribution and -delivery do: load them into two databases")
	case o.controllerSession != "" && !o.failures:
		return errors.New("-controller-session names the controller of -failures' projects: name -failures too")
	}
	return nil
}

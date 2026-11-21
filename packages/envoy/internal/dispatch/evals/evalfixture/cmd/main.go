// Command evalfixture loads the histories the eval registry's checks read (dispatch://INFRA-2916,
// k6, k12, k14, k15 and k17) into a database that already holds Dispatch's schema, and prints, as
// JSON, what each mode wrote: the project, the counts, the times a check reads its windows from,
// for -attribution each row with the projects it counts for, and the seed where -delivery, the
// one seeded mode, ran.
//
//	go run ./internal/dispatch/evals/evalfixture/cmd -database <url> -project LEGK \
//	  -legion -issues 240 -now 2026-11-13T12:00:00Z
//	go run ./internal/dispatch/evals/evalfixture/cmd -database <url> -project DELV \
//	  -delivery -days 28 -seed 1 -now 2026-11-13T12:00:00Z
//	go run ./internal/dispatch/evals/evalfixture/cmd -database <url> \
//	  -merge-pr acme/api#12 -now 2026-11-13T13:00:00Z
//
// The modes add to one project and write its projects row only where it has none, so they
// combine; after them the project's next issue number is moved past every issue it holds. Every
// mode but -merge-pr writes into -project, which it requires; -merge-pr names its pull request by
// repository and number and writes no project's rows, so it needs none:
//
//	-legion       legiontest's Legion history, at least -issues finished issues, observed from
//	              partway through, and the project's legion_settings row (this head's schema)
//	-base         the same history into a database of the base head 02a7b3c6 (k14); alone
//	-attribution  k15's case under -project, -project+"B" and -project+"C"; not with -delivery,
//	              since both write the one delivery_settings row
//	-delivery     -days days of deliverytest's delivery history, seeded by -seed
//	-merge-pr     <owner>/<repo>#<n>: that pull request merged at -now (k12's one change)
//
// Every time is relative to -now, which defaults to this second: two loads with one -now hold
// the same history at the same times.
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
	delivery                   bool
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
}

func main() {
	var o options
	var now string
	flag.StringVar(&o.database, "database", "", "the Postgres URL to write into; it must hold Dispatch's schema")
	flag.StringVar(&o.project, "project", "", "the project every mode writes into; required by every mode but -merge-pr, which writes no project's rows")
	flag.BoolVar(&o.legion, "legion", false, "write the Legion history and the project's legion_settings row")
	flag.BoolVar(&o.base, "base", false, "write the Legion history into a database of the base head 02a7b3c6")
	flag.BoolVar(&o.attribution, "attribution", false, "write k15's attribution case")
	flag.BoolVar(&o.delivery, "delivery", false, "write -days days of delivery rows for the project")
	flag.StringVar(&o.mergePR, "merge-pr", "", "merge the pull request <owner>/<repo>#<n> at -now")
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
	case o.project == "" && (o.legion || o.base || o.attribution || o.delivery):
		return errors.New("name the project -legion, -base, -attribution and -delivery write into with -project; -merge-pr alone needs none")
	case o.database == "":
		return errors.New("name the database to write with -database <url>")
	case !o.legion && !o.base && !o.attribution && !o.delivery && o.mergePR == "":
		return errors.New("name at least one mode: -legion, -base, -attribution, -delivery or -merge-pr")
	case o.base && (o.legion || o.attribution || o.delivery || o.mergePR != ""):
		return errors.New("-base writes the Legion history alone, into a database of the base head")
	case o.attribution && o.delivery:
		return errors.New("-attribution and -delivery each write the one delivery_settings row: load them into two databases")
	}
	return nil
}

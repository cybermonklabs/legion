package tmux

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Owned windows the daemon does not know, from `list-windows -F
// "#{window_id}\t#{@legion_owner}\t#{window_activity}"` (tmux.ts:455-489): a window marked with
// another owner, or with none, is someone else's and never a candidate.
func TestReadUnknownWindows(t *testing.T) {
	stdout := "@0\t\t1790103302\n" + // the bootstrap window: no owner marker
		"@1\tlegion-omp\t1790103302\n" + // known
		"@2\tlegion-omp\t1790100000\n" + // an orphan
		"@3\tlegion-other\t1790103302\n" + // another daemon's
		"@4\tlegion-omp\tnot-a-time\n" + // cannot be timed: never reaped
		"garbage\n"
	got := readUnknownWindows(stdout, "legion-omp", map[string]bool{"@1": true})
	want := []ownedWindow{{id: "@2", activity: time.Unix(1790100000, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readUnknownWindows = %+v, want %+v", got, want)
	}
}

// Unknown worker-shim panes anywhere on the server, in owned windows (tmux.ts:491-545). The row
// shape is tmux 3.7c's own rendering of a `/bin/sh -c` pane (captured on the devbox): the start
// command's newlines and tabs are printed escaped, and the activity column is the window's.
func TestReadUnknownPanes(t *testing.T) {
	stdout := "%0\t@0\t\t\"sleep 3600\"\t1790103321\n" +
		"%1\t@1\tlegion-omp\t/bin/sh -c \"cd /w && /opt/legion worker-shim --connect unix:///s -- omp \\\"a\\n\\nb\\ttab\\\"\"\t1790103321\n" +
		"%2\t@1\tlegion-omp\t/bin/sh -c \"cd /w && /opt/legion worker-shim --connect unix:///s -- omp\"\t1790103322\n" +
		"%3\t@1\tlegion-omp\tbash\t1790103322\n" + // not a Legion process: never reaped here
		"%4\t@5\tlegion-other\t/bin/sh -c \"legion worker-shim\"\t1790103322\n" + // another daemon's
		"%5\t@1\tlegion-omp\t/bin/sh -c \"legion worker-shim\"\t\n" // cannot be timed
	got := readUnknownPanes(stdout, "legion-omp", map[string]bool{"%2": true})
	want := []ownedPane{{id: "%1", window: "@1", activity: time.Unix(1790103321, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readUnknownPanes = %+v, want %+v", got, want)
	}
}

// tmux floors a window's activity to the whole second, and output a pane writes while a sweep's
// listing runs can move it into a later second than the moment the sweep began. A sweep with grace
// 0, as boot runs it, still reaps the orphan window and the orphan pane such listings show: each is
// aged against the runtime's clock read after the listing that showed it. The stand-in tmux server
// moves the runtime's clock past a second boundary as it answers each listing, and lists the
// activity as that new second: the window pass in the second after the sweep began, the pane pass
// in the second after that, so a clock read once for both passes fails the pane pass too.
func TestAGraceZeroSweepReapsOrphansWhoseActivityIsALaterSecondThanTheSweepBegan(t *testing.T) {
	const socket = "legion-omp"
	began := time.Date(2026, 11, 16, 6, 35, 30, 980_000_000, time.UTC)
	windowsSecond, panesSecond := began.Truncate(time.Second).Add(time.Second), began.Truncate(time.Second).Add(2*time.Second)
	// The runtime's clock, which only the stand-in server below moves.
	moment := began
	r := &Runtime{
		socket:    socket,
		now:       func() time.Time { return moment },
		log:       slog.New(slog.DiscardHandler),
		tracked:   map[string]*trackedProcess{},
		displaced: map[claim.Token]runtime.Locator{},
	}
	// The pane the runtime watches keeps window @2 known, so the orphan pane %3 beside it is the
	// pane pass's alone.
	r.track(runtime.Locator{
		Runtime: runtime.RuntimeTmux, Claim: "legion-omp-legion-1-architect", Incarnation: "4242:1",
		Tmux: &runtime.TmuxLocator{Window: "@2", Pane: "%2"},
	}, "LEGION-1", ghrepo.Repository{}, "", claim.RoleArchitect)
	var kills [][]string
	r.run = func(_ context.Context, argv []string) (result, error) {
		switch verb(argv) {
		case "list-windows":
			moment = windowsSecond.Add(10 * time.Millisecond)
			return result{stdout: fmt.Sprintf("@1\t%[1]s\t%[2]d\n@2\t%[1]s\t%[2]d\n", socket, windowsSecond.Unix())}, nil
		case "list-panes":
			moment = panesSecond.Add(10 * time.Millisecond)
			return result{stdout: fmt.Sprintf("%%2\t@2\t%[1]s\t/bin/sh -c \"legion worker-shim\"\t%[2]d\n"+
				"%%3\t@2\t%[1]s\t/bin/sh -c \"legion worker-shim\"\t%[2]d\n", socket, panesSecond.Unix())}, nil
		case "kill-window", "kill-pane":
			kills = append(kills, argv)
			return result{}, nil
		}
		return result{}, fmt.Errorf("the stand-in tmux server answers no %q", argv)
	}
	if err := r.ReconcileOrphans(context.Background(), nil, 0); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if want := [][]string{killWindowArgv(socket, "@1"), killPaneArgv(socket, "%3")}; !reflect.DeepEqual(kills, want) {
		t.Errorf("a sweep with grace 0 begun at %s killed %q, want the orphan window listed active at %s and the orphan pane listed active at %s, %q",
			began.Format(time.RFC3339Nano), kills, windowsSecond.Format(time.RFC3339), panesSecond.Format(time.RFC3339), want)
	}
}

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The start of a next change's working copy at its branch goes over the claim's connection to the
// pane's shim, within the runtime's budget, once the pane verifies as the recorded process, and
// answers what the shim answered. A shim's refusal comes back as that refusal. A pane that runs
// another process is asked nothing: the connection is the recorded process's.
func TestStartAtBranchAsksThePanesShim(t *testing.T) {
	conns := fake.NewConns()
	f := newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) { o.Conns = conns })
	f.run = func(_ context.Context, argv []string) (result, error) {
		if verb(argv) == "list-panes" {
			return result{stdout: "%1 4242\n"}, nil
		}
		return result{}, nil
	}
	f.readProc = func(path string) ([]byte, error) {
		switch path {
		case "/proc/4242/stat":
			return []byte("4242 (sh) " + statTail), nil
		case "/proc/4242/cmdline":
			return []byte(livePaneOmp), nil
		}
		return nil, os.ErrNotExist
	}
	token := "legion-omp-ACME-1-implementer"
	loc := runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: claim.Token(token), Incarnation: "4242:1234567",
		Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}
	conn := fake.NewConn()
	conns.Register(loc.Claim, conn)
	at := runtime.StartAt{Bookmark: "legion/ACME-1", Base: "main"}
	answer := runtime.StartedAt{Moved: true, From: strings.Repeat("a", 40), At: strings.Repeat("b", 40), BehindBase: true}
	conn.AnswerStartAtBranch(answer)

	started, err := f.StartAtBranch(context.Background(), loc, at)
	if err != nil || started != answer {
		t.Fatalf("StartAtBranch = %+v, %v; want the shim's %+v", started, err, answer)
	}
	if asked := conn.StartAts(); len(asked) != 1 || asked[0].StartAt != at || asked[0].Timeout != runtime.StartAtBranchTimeout {
		t.Fatalf("starts asked %+v, want one of %+v within the runtime's budget", asked, at)
	}

	conn.FailStartAtBranch(fmt.Errorf("worker-stream: refused start-at-branch: %w", runtime.ErrStartAtRefused))
	if _, err := f.StartAtBranch(context.Background(), loc, at); !errors.Is(err, runtime.ErrStartAtRefused) || !strings.Contains(err.Error(), token) {
		t.Fatalf("StartAtBranch the shim refused = %v, want runtime.ErrStartAtRefused naming the claim", err)
	}

	other := loc
	other.Incarnation = "4242:1"
	if _, err := f.StartAtBranch(context.Background(), other, at); err == nil || errors.Is(err, runtime.ErrStartAtRefused) {
		t.Fatalf("StartAtBranch of a pane that runs another process = %v, want a refusal of the runtime's own", err)
	}
	if asked := conn.StartAts(); len(asked) != 2 {
		t.Fatalf("starts asked %+v, want none of a pane that runs another process", asked)
	}
}

package daemon

import (
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A launch that a runtime's own check of its arm's folder sends to its fallback reaches the daemon
// through the hook the daemon hands both runtimes (tmux.Options.TextsFellBack and
// sandbox.Options.TextsFellBack, both textsReports.fellBack): under tmux a plugin text the folder
// replaces at a stale digest; under kubernetes a launcher that refused the folder, one that cannot
// check a folder, and a launch that installs Legion's plugins after its start. The runtime reports
// what it launched in place of the folder (runtime.SpawnSpec.FallBack): the arm's fallback, with no
// folder. For each, the daemon logs one line naming the claim, the generation, the experiment, the
// arm, the folder, the file (none for a cause of the folder as a whole) and the cause, as it logs
// a fallback its own check finds, and applies one fact, which tells the issue.
func TestARuntimeFallbackIsLoggedAndNoticedWithItsExperimentAndArm(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	const folder = "/etc/legion/texts/shorter"
	for n, tc := range []struct {
		name  string
		fault texts.Fault
	}{
		{"a tmux pane whose plugin text the folder replaces is at another digest", texts.Fault{Path: "agents/goal-red-team.md", Cause: texts.DigestDiffers,
			Detail: "manifest.json records sha256:" + strings.Repeat("1", 64) + ", and Legion's text is sha256:" + strings.Repeat("2", 64)}},
		{"a pod's launcher that refused the folder", texts.Fault{Dir: folder, Path: "skills/legion-worker/SKILL.md", Cause: texts.NameDiffers,
			Detail: "its frontmatter names legion-work"}},
		{"a pod's launcher that cannot check a folder", texts.Fault{Cause: texts.LauncherCannotCheck}},
		{"a launch that installs Legion's plugins after its start", texts.Fault{Cause: texts.InstallsPlugins}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			issue := fmt.Sprintf("LEGION-%d", 300+n)
			seedTextsIssue(t, pool, issue)
			logs := &syncBuffer{}
			reports := &textsReports{log: slog.New(slog.NewJSONHandler(logs, nil))}
			reports.bind(pool, textsHandlers(t, pool))
			hook := runtime.TextsFellBack(reports.fellBack)

			token := mustClaimToken(t, issue, claim.RoleReviewer)
			arm := runtime.Variant{Experiment: "shorter", Name: "folder", ConfigFiles: []string{"/etc/legion/shorter.yml"}, Texts: folder}
			fallback := runtime.TextsFallback{Prompt: runtime.PromptParts{RolePromptPaths: []string{"/state/prompts/shared/reviewer.md"}}}
			spec := runtime.SpawnSpec{Claim: token, Project: textsProject, Tree: issue, Issue: issue, Role: claim.RoleReviewer, Generation: 6,
				Prompt:   runtime.PromptParts{RolePromptPaths: []string{"/state/texts/" + string(token) + "/6/folder/roles/reviewer.md"}, VariantInstructionsPaths: []string{"/state/experiments/shorter/folder.md"}},
				Variants: []runtime.Variant{arm},
				Texts: &runtime.Texts{Folder: folder, Fingerprint: strings.Repeat("ab", 32), Copy: "/state/texts/" + string(token) + "/6/folder",
					Experiment: "shorter", Variant: "folder", Fallback: fallback}}
			launched, report := spec.FallBack(tc.fault)
			if launched.Texts != nil || !reflect.DeepEqual(launched.Prompt, fallback.Prompt) || launched.Variants != nil {
				t.Fatalf("the runtime launches %+v in %+v with the folder %+v, want the arm's fallback", launched.Prompt, launched.Variants, launched.Texts)
			}
			hook(report)

			lines := loggedMessages(t, logs.String(), textsFallbackLine)
			if len(lines) != 1 {
				t.Fatalf("the daemon logged %d fallbacks, want one: %s", len(lines), logs.String())
			}
			file := tc.fault.Path
			if file == "" {
				file = "none"
			}
			for key, value := range map[string]any{"claim": string(token), "generation": float64(6), "experiment": "shorter", "arm": "folder",
				"folder": folder, "file": file, "cause": string(tc.fault.Cause), "fingerprint": strings.Repeat("ab", 32)} {
				if lines[0][key] != value {
					t.Errorf("the fallback line's %s = %v, want %v (line %v)", key, lines[0][key], value, lines[0])
				}
			}

			r := &textsRig{t: t, logs: logs}
			r.awaitNotices(token, 1)
			if got := textsFacts(t, pool, token); got != 1 {
				t.Errorf("intake applied %d fallback facts of %s, want one", got, token)
			}
			messages := textsMessages(t, pool, issue)
			if len(messages) != 1 {
				t.Fatalf("the issue was told %q, want one message", messages)
			}
			for _, name := range []string{fmt.Sprintf("claim %s at generation 6", token), "experiment " + strconv.Quote("shorter"),
				"arm " + strconv.Quote("folder"), strconv.Quote(folder), "the cause " + strconv.Quote(string(tc.fault.Cause))} {
				if !strings.Contains(messages[0], name) {
					t.Errorf("the message is %q, want it to name %s", messages[0], name)
				}
			}
		})
	}
}

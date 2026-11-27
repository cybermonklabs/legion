package textcheck

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// Pairs of operator-guide sentences that disagreed, each now worded as the code decides, and the
// sentence each one replaced is gone from its text. Each text is read as prose (proseOf), so a
// line break or a comment's `#` decides nothing. Each case names the code its sentences state:
//   - an App's credential: an App takes exactly one of four sources, token_command among them,
//     and one with token_command takes no installations (config's readGitHubApp, appSources);
//   - a role's value: a role must name its type and its prompt file, and may name its model and
//     its App (config's readRole);
//   - the shared-box runbook's App keys: the loader takes every source, so the runbook's own
//     choice of token_command is the runbook's, not the daemon's;
//   - a session's measurements: a process measures at most CAPABILITY_ATTEMPTS times
//     (pi-legion's capability-schedule.ts);
//   - a new overlay mount: an issue pod that lacks it has drifted (driftedInPod, PodDrift) and its
//     update round replaces it, its roles alive meanwhile, and only a pod no round covers, the
//     controller's, reads StaleAddress (movedInPod);
//   - a rename under a role's home: the home overlay is mounted with userxattr, so the rename no
//     longer panics gVisor (launcher's homeOverlayOptions);
//   - a root's label taken off: a running tree goes on, and a root awaiting a person leaves the
//     workflow (admit's releaseUnslotted, awaitingUnresumable);
//   - a repository without Dispatch's App: the daemon's read of an issue's pull request at its
//     start and after each handoff still applies what GitHub shows (daemon's readIssuePullRequest).
func TestTheOperatorGuidesAgreeOnEachPairAsTheCodeDoes(t *testing.T) {
	const (
		runningLegion   = "docs/site/src/content/docs/legion/running-legion.md"
		usingLegion     = "docs/site/src/content/docs/legion/using-legion.md"
		troubleshooting = "docs/site/src/content/docs/legion/troubleshooting.md"
		deployment      = "docs/deployment.md"
		kubernetes      = "docs/kubernetes.md"
		testingGuide    = "docs/testing.md"
	)
	type text struct {
		path       string
		want, gone []string
	}
	for _, tc := range []struct {
		name  string
		texts []text
	}{
		{
			name: "an App takes exactly one of four credential sources",
			texts: []text{{
				path: runningLegion,
				want: []string{
					"each App takes exactly one of `private_key` (the PEM itself), `private_key_command` (a command whose output is the PEM), `private_key_secret` or `token_command`.",
					"With `token_command` the daemon holds no key and runs the command for each lease it needs.",
					"`installations` (owner to installation id) is optional, and an App with `token_command` takes none, since the command's release names each installation.",
				},
				gone: []string{"(a command whose output is the PEM) or `private_key_secret`. The daemon"},
			}},
		},
		{
			name: "a role must name its type and its prompt file alone",
			texts: []text{{
				path: deployment,
				want: []string{"Each value holds the role's `type` and `prompt_file`, and may hold its `model` and `github_app`."},
				gone: []string{"Each value holds the role's `type`, `prompt_file`, `model` and `github_app`."},
			}},
		},
		{
			name: "the shared-box runbook's choice of token_command is its own",
			texts: []text{{
				path: deployment,
				want: []string{
					"This runbook's daemon uses no raw key secret.",
					"Each App needs its own `app_id` and exactly one credential source, and this runbook's is `token_command`.",
				},
				gone: []string{"Raw key secrets are not used.", "with no second credential source"},
			}},
		},
		{
			name: "a session measures at most three times",
			texts: []text{{
				path: kubernetes,
				want: []string{
					"A process measures at most three times.",
					"So a session whose three measurements all fell in a Dispatch, GitHub or search-provider outage reads open until its next boot.",
				},
				gone: []string{"Nothing measures again mid-session"},
			}},
		},
		{
			name: "a new overlay mount opens an update round on an issue pod",
			texts: []text{
				{
					path: runningLegion,
					want: []string{
						"Adding the mount to a running deployment opens an update round on every affected issue pod after the restart.",
						"The round asks each of its roles, named or not, to push its work and answer `ready_for_update`, and then replaces the pod.",
					},
					gone: []string{"replaces every affected issue pod at the restart: each of its running roles, named or not, loses its turn"},
				},
				{
					path: kubernetes,
					want: []string{
						"The sweep opens the pod's update round, as for any drift of a pod, and each role keeps running until the round replaces the pod.",
						"Where the key names the controller, its pod, which no round covers, is reported `StaleAddress` at the boot's re-adoption instead.",
					},
					gone: []string{
						"every role of an affected pod that is running is reported `StaleAddress` at the boot's re-adoption",
						"and a role mid-turn at the boot's re-adoption is reported `StaleAddress` and relaunched at once",
					},
				},
			},
		},
		{
			name: "a rename under a role's home panicked gVisor only before userxattr",
			texts: []text{{
				path: testingGuide,
				want: []string{
					"A directory rename under `$HOME` panicked the gVisor sandbox of a Legion pod and killed every role in it.",
					"Since legion-v19.18.1 each role's home overlay is mounted with `userxattr`, and the rename no longer does",
					"So under an earlier release the permission holds only while `XDG_CACHE_HOME` is honoured",
				},
				gone: []string{"panics the gVisor sandbox of a Legion pod"},
			}},
		},
		{
			name: "taking the label off takes out a root that waits on a person",
			texts: []text{{
				path: usingLegion,
				want: []string{"Taking the label off alone does not stop a tree at work, but a root that waits on a person then leaves the workflow (`phase: done`)."},
				gone: []string{"taking the label off alone does not stop a tree that has started."},
			}},
		},
		{
			name: "the read of an issue's pull request brings changes in without the webhook",
			texts: []text{{
				path: troubleshooting,
				want: []string{
					"Its pull requests' changes come in only through the daemon's read of an issue's pull request, at the daemon's start and after every handoff",
					"So an issue that waits on a GitHub event stands still until the next such read.",
				},
				gone: []string{"nothing brings its pull requests' changes in"},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, text := range tc.texts {
				prose := proseOf(shippedText(t, text.path))
				for _, phrase := range text.want {
					if !strings.Contains(prose, phrase) {
						t.Errorf("%s omits %q", text.path, phrase)
					}
				}
				for _, phrase := range text.gone {
					if strings.Contains(prose, phrase) {
						t.Errorf("%s still says %q", text.path, phrase)
					}
				}
			}
		})
	}
}

// The configuration reference (`legion config reference --json`, config.ConfigReference) says
// what readGitHubApp and readRole take: an App's one credential may be a token_command, and a
// role's model and App are optional.
func TestTheConfigReferenceSaysWhatAnAppAndARoleTake(t *testing.T) {
	descriptions := map[string]string{}
	for _, key := range config.ConfigReference() {
		descriptions[key.Name] = key.Description
	}
	for _, tc := range []struct{ key, want, gone string }{
		{
			key:  "github_apps",
			want: "each with its id and exactly one credential: a private key, from which the daemon mints every agent's GitHub token, or a `token_command` that prints those tokens.",
			gone: "each with its id and its private key",
		},
		{
			key:  "roles",
			want: "each value the role's `type` and `prompt_file`, and its optional `model` and `github_app`.",
			gone: "each value the role's `type`, `prompt_file`, `model` and `github_app`.",
		},
	} {
		description, ok := descriptions[tc.key]
		if !ok {
			t.Errorf("the configuration reference has no key %s", tc.key)
			continue
		}
		if !strings.Contains(description, tc.want) {
			t.Errorf("%s's description %q omits %q", tc.key, description, tc.want)
		}
		if strings.Contains(description, tc.gone) {
			t.Errorf("%s's description still says %q", tc.key, tc.gone)
		}
	}
}

// Stage 4b's `done` checkpoint finds nothing of tree 1 in the workflow at the merge: the
// children-first-acceptance checkpoint parks children 3 and 4 before the sign-off, which the
// daemon refuses while an issue beneath the root is in the workflow (SIGNOFF_CHILD_IN_WORKFLOW).
// So the merge's own close runs (workflow's closeMerged), and the only wait it can log is
// "workflow: a merged issue's close waits for the issues beneath it". The README and the driver
// say so, and neither names the earlier log lines, which no daemon code writes now.
func TestTheStage4bDoneCheckpointHasTheMergeCloseTree1AtOnce(t *testing.T) {
	const wait = "workflow: a merged issue's close waits for the issues beneath it"
	gone := []string{"a merged root's close waits for the rest of its tree", "a merged root closes once its last child left the workflow"}
	readme := proseOf(shippedText(t, "scripts/e2e/README.md"))
	for _, phrase := range []string{
		"**The merge closes tree 1 at once**, since nothing of its tree is in the workflow.",
		"So no `" + wait + "` line names tree 1, and its architect gets no `pr-merged` notice, which is that wait's own:",
	} {
		if !strings.Contains(readme, phrase) {
			t.Errorf("scripts/e2e/README.md omits %q", phrase)
		}
	}
	driver := shippedText(t, "scripts/e2e/stage4b-sandbox-tree.sh")
	if want := `merge_waits=$(log_lines "` + wait + `"`; !strings.Contains(driver, want) {
		t.Errorf("scripts/e2e/stage4b-sandbox-tree.sh omits %q", want)
	}
	for _, phrase := range append(gone, "**The merge does not close tree 1**") {
		if strings.Contains(readme, phrase) {
			t.Errorf("scripts/e2e/README.md still says %q", phrase)
		}
	}
	for _, phrase := range gone {
		if strings.Contains(driver, phrase) {
			t.Errorf("scripts/e2e/stage4b-sandbox-tree.sh still waits for %q", phrase)
		}
	}
}

package textcheck

import (
	"strings"
	"testing"
)

// Nine sentences of the operator guides each say what the code does, and the sentence each one
// replaced is gone from its text. Each text is read as prose (proseOf), so a line break or a
// comment's `#` decides nothing. Each case names the code its sentence states:
//   - a role's model: a role of `roles` or of an issue's plan that names a `model` is launched on
//     it, and any other role on `@default` (daemon's workerSpawnSpec sets SpawnSpec.Model from the
//     role's one resolution, resolveClaimRole, sandbox's agentArgv);
//   - the daemon's controller: `controller: daemon` launches a pane of its own window under tmux
//     too (runtime/tmux's controllerWindow);
//   - a review bot's red: a CI settlement moves no phase and tells three roles (workflow's
//     tellChecks), and the reviewer answers each bot thread (roles/core/reviewer.md);
//   - a role's gh leases: every directory of a role's bundle holds the role's own App lease, and
//     only a controller's bundle holds read leases (appauth's GitHubCredential, mintBundle);
//   - a renamed repository's leases: a rename's old owner's directory holds the serving owner's
//     lease only where the App is not installed on that old owner, and the bundle's pairs route
//     the moved repository to the serving owner's directory (appauth's bundleDirs, cmd/legion's
//     ghConfigDirFor);
//   - a merged root's wait: its phase stays merging while its Dispatch status is retro
//     (workflow's closeMerged, phaseStatus);
//   - a resumed session's model: every pod launch names `--model` (sandbox's agentArgv), and the
//     pinned Oh My Pi restores a saved model only for a launch that names none;
//   - the roles the boot gate refuses: an unset `smol` or `slow` takes the `default` role's model,
//     and a missing `default` is refused nowhere (probe.mjs's agentModels);
//   - a merged child's pod: a child closed by its merge where a production check opens keeps its
//     pod until the check's first look ends (workflow's leave, endLookPending, closeCheck).
func TestTheOperatorGuidesSayWhatTheCodeDoes(t *testing.T) {
	const (
		runningLegion = "docs/site/src/content/docs/legion/running-legion.md"
		concepts      = "docs/site/src/content/docs/legion/concepts.md"
		usingLegion   = "docs/site/src/content/docs/legion/using-legion.md"
		kubernetes    = "docs/kubernetes.md"
		routeReadme   = "deploy/kubernetes/operator-route/README.md"
		routeOverlay  = "deploy/kubernetes/operator-route/overlay.yml"
		daemonDesign  = ".dispatch/architecture/daemon.md"
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
			name: "a role of roles or of a plan that names a model runs on it",
			texts: []text{
				{
					path: runningLegion,
					want: []string{
						"A role whose `roles.<name>.model` or whose plan's role block names a `model` runs on that model, in place of the `default` line.",
					},
					gone: []string{
						"A role of `roles` that names a `model` runs on that `model` in `legion.yaml`, in place of the `default` line.",
					},
				},
				{
					path: routeReadme,
					want: []string{
						"A role whose settings or plan block name a `model` runs on that model, named in `legion.yaml` or the issue's plan, in place of the `default` line.",
					},
					gone: []string{
						"A role of `roles` that names a `model` runs on that `model` in `legion.yaml`, in place of the `default` line.",
					},
				},
				{
					path: routeOverlay,
					want: []string{
						"a role whose legion.yaml settings (roles.<name>.model) or plan block names a model is launched with --model and that model.",
					},
					gone: []string{
						"A role of roles that names a model runs on that model in legion.yaml, in place of the default line.",
					},
				},
				{
					path: kubernetes,
					want: []string{
						"A role of `roles` that names a `model` runs on that `model` in `legion.yaml`, in place of the `default` line.",
					},
					gone: []string{
						"A role of `roles` that names a `model` runs on it, and its line under `modelRoles` does not apply to it.",
						"and no line under `modelRoles` applies to it.",
					},
				},
			},
		},
		{
			name: "the daemon launches the controller under tmux too",
			texts: []text{
				{
					path: concepts,
					want: []string{"Under `controller: daemon` the daemon launches it and relaunches it when it dies, as a pod in the cluster or a pane under tmux."},
					gone: []string{"Under `controller: daemon` (Kubernetes only)"},
				},
				{
					path: runningLegion,
					want: []string{"Under `controller: daemon` the daemon launches it and keeps it running: as a pod in the cluster, or as a pane under `runtime: tmux`."},
					gone: []string{"or in the cluster, as a pod the daemon launches and keeps running (`controller: daemon`)."},
				},
			},
		},
		{
			name: "a review bot's red moves no phase",
			texts: []text{{
				path: runningLegion,
				want: []string{
					"A review bot's red is now CI like any other, and it moves no phase.",
					"The daemon names it to the agent of the implementer's step, to the worker of the issue's phase and to the architect that owns the issue.",
					"Where the task has a review step, the reviewer also answers each bot thread, and a finding it cannot accept becomes its own.",
				},
				gone: []string{
					"it sends the work back to the implementer",
					"the reviewer adjudicates nothing of it and re-runs nothing",
				},
			}},
		},
		{
			name: "every directory of a role's bundle holds the role's own lease",
			texts: []text{{
				path: daemonDesign,
				want: []string{
					"Each directory of a role's bundle holds the lease of the role's App on the owner it is named for, with the role's permissions.",
					"A controller's bundle holds the read lease of the same owners.",
				},
				gone: []string{
					"every other owner the role's App is installed on keeps one read lease on its own installation",
					"the write lease for the role's own repository stands in two directories and no others",
				},
			}},
		},
		{
			name: "a resolving rename routes the moved repository and leaves each installed owner its own lease",
			texts: []text{{
				path: daemonDesign,
				want: []string{
					"A resolving pair changes where the moved repository is answered from.",
					"Where the role's App is not installed on the recorded owner, that owner's directory holds the lease of the owner serving the repository.",
					"Where the App is installed on both owners, each directory holds its own owner's lease.",
				},
				gone: []string{"the directory named for the recorded owner and the directory of the owner that serves that repository now hold one lease"},
			}},
		},
		{
			name: "a merged root waits in the status retro and the phase merging",
			texts: []text{{
				path: usingLegion,
				want: []string{
					"A root whose tree still runs stays here after its merge too, in the phase `merging`, until its close.",
					"A root whose tree still runs waits in the status `retro`, its phase `merging`, until the rest of its tree leaves the workflow.",
				},
				gone: []string{
					"through the wait between its merge and its close",
					"A root whose tree is still running waits in `retro` until the last of it is done",
				},
			}},
		},
		{
			name: "a resumed session runs the model its launch names",
			texts: []text{
				{
					path: routeReadme,
					want: []string{
						"An older model can leave all three once no role, no plan and no fallback chain names it.",
						"A resumed session does not need it, since each launch names its model with `--model`.",
					},
					gone: []string{"a resumed session restores its own model", "once no role and no fallback chain names it"},
				},
				{
					path: kubernetes,
					want: []string{
						"A resumed Oh My Pi session runs the model its launch names, not the one it last used.",
						"that drops a model a session last used fails none of its resumes.",
					},
					gone: []string{
						"A resumed session also restores the model it last used",
						"Keep the route unchanged across the copy below",
					},
				},
			},
		},
		{
			name: "the boot gate refuses an agent's unset role and no missing default",
			texts: []text{{
				path: runningLegion,
				want: []string{
					"The task agents Legion's prompts dispatch take the models of `review`, `oracle`, `deep`, `smol` and `slow`.",
					"The daemon refuses to boot when one of those roles has no model, or its model's key does not work.",
					"`smol` and `slow` left unset take the `default` role's model, which the daemon accepts.",
					"Nothing refuses a missing `default`. An Oh My Pi session whose role names no model then starts, silently, on one of Oh My Pi's built-in models.",
				},
				gone: []string{
					"and the rest the example names): the daemon refuses to boot when an agent's role has no model",
					"The daemon refuses to boot when an agent's role has no model",
					"Nothing refuses a missing `default`: an",
				},
			}},
		},
		{
			name: "a merged child's pod waits for its production check's first look",
			texts: []text{{
				path: concepts,
				want: []string{"Where the close is the child's merge and a production check opens, the pod and its disk go when the check's first look ends."},
				gone: []string{"A child's close deletes its pod, and the disk goes with it; a root's close"},
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

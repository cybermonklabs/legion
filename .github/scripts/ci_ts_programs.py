"""The pairs check-ts-filters.sh derives the TypeScript half of each Bun gate's reads from: a
filter of ci.yaml's changes job and the tsc project the job that filter gates typechecks. The
script's header says what it proves, which gates it leaves out and why.

Each entry is (filter, project, why):
  filter   a filter of the changes job, the gate of the job whose typecheck this program is
  project  what `bunx tsc -p` takes: the directory holding the tsconfig.json the job's typecheck
           reads, or the project file itself where the job's step names another one
  why      the step the pair stands for, read from that job's own steps and named with the file
           that holds them — ci.yaml's job of that id keeps the gate and calls it — which the
           failure message carries so the reader knows which typecheck holds the file

The pairs are hand-kept and few; the files of each program are derived, which is the point. A
job that typechecks a second project is a second entry (the envoy-client job typechecks
broker-client beside its own package, the dispatch-ui job the proof editor beside the dashboard,
and the test job the three script directories that are no workspace).
"""

PROGRAMS = [
    ("contracts", "packages/contracts",
     "ci-job-contracts.yaml's contracts job: Typecheck contracts (bun run typecheck: bunx tsc --noEmit)"),
    ("envoy_client", "packages/envoy-client", "ci-job-envoy-client.yaml's envoy-client job: Typecheck Envoy client"),
    ("envoy_client", "packages/broker-client", "ci-job-envoy-client.yaml's envoy-client job: Typecheck broker client"),
    ("pi_shared", "packages/pi-shared", "ci-job-pi-shared.yaml's pi-shared job: Typecheck pi-shared"),
    ("pi_envoy", "packages/pi-envoy", "ci-job-pi-envoy.yaml's pi-envoy job: Typecheck pi-envoy"),
    ("pi_legion", "packages/pi-legion", "ci-job-pi-legion.yaml's pi-legion job: Typecheck pi-legion"),
    ("claude_envoy", "packages/claude-envoy",
     "ci-job-claude-envoy.yaml's claude-envoy job: Typecheck Claude Envoy plugin"),
    ("dispatch_ui", "packages/dispatch", "ci-job-dispatch-ui.yaml's dispatch-ui job: Typecheck dispatch dashboard"),
    # The proof editor's and the docs site's steps run the same `bun run typecheck`, whose recipe
    # names a project file of its own rather than the directory's tsconfig.json.
    ("dispatch_ui", "packages/proof-editor/tsconfig.check.json",
     "ci-job-dispatch-ui.yaml's dispatch-ui job: Typecheck proof editor "
     "(bun run typecheck: bunx tsc --noEmit -p tsconfig.check.json)"),
    ("docs", "docs/site/tsconfig.tools.json",
     "docs-site.yaml's build job: Typecheck the docs site (bun run typecheck: tsc --noEmit -p tsconfig.tools.json), "
     "which ci.yaml's docs job calls"),
    ("conflicts_script", "scripts/conflicts",
     "ci-job-conflicts-script.yaml's conflicts-script job: Typecheck the conflict measurement script"),
    ("test", "scripts/dispatch",
     "ci-job-test.yaml's test job: Typecheck the Dispatch census script (bunx tsc --noEmit -p scripts/dispatch)"),
    ("test", "scripts/github-apps",
     "ci-job-test.yaml's test job: Typecheck the GitHub App registration script "
     "(bunx tsc --noEmit -p scripts/github-apps)"),
    ("test", "scripts/sessions",
     "ci-job-test.yaml's test job: Typecheck the session message-wait script "
     "(bunx tsc --noEmit -p scripts/sessions)"),
]

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
and the test job the four script directories that are no workspace).
"""


def at(job: str, step: str) -> str:
    """A `why` naming a step of a ci.yaml job, in the file that holds it: ci.yaml's job of that
    id keeps the gate and calls .github/workflows/ci-job-<id>.yaml, where the steps are. A job
    whose steps are in a file of another name writes its own `why`, as the docs row does."""
    return f"ci-job-{job}.yaml's {job} job: {step}"


PROGRAMS = [
    ("contracts", "packages/contracts", at("contracts", "Typecheck contracts (bun run typecheck: bunx tsc --noEmit)")),
    ("envoy_client", "packages/envoy-client", at("envoy-client", "Typecheck Envoy client")),
    ("envoy_client", "packages/broker-client", at("envoy-client", "Typecheck broker client")),
    ("pi_shared", "packages/pi-shared", at("pi-shared", "Typecheck pi-shared")),
    ("pi_envoy", "packages/pi-envoy", at("pi-envoy", "Typecheck pi-envoy")),
    ("pi_legion", "packages/pi-legion", at("pi-legion", "Typecheck pi-legion")),
    ("claude_envoy", "packages/claude-envoy", at("claude-envoy", "Typecheck Claude Envoy plugin")),
    ("dispatch_ui", "packages/dispatch", at("dispatch-ui", "Typecheck dispatch dashboard")),
    # The proof editor's and the docs site's steps run the same `bun run typecheck`, whose recipe
    # names a project file of its own rather than the directory's tsconfig.json.
    ("dispatch_ui", "packages/proof-editor/tsconfig.check.json",
     at("dispatch-ui", "Typecheck proof editor (bun run typecheck: bunx tsc --noEmit -p tsconfig.check.json)")),
    ("docs", "docs/site/tsconfig.tools.json",
     "docs-site.yaml's build job: Typecheck the docs site (bun run typecheck: tsc --noEmit -p tsconfig.tools.json), "
     "which ci.yaml's docs job calls"),
    ("conflicts_script", "scripts/conflicts", at("conflicts-script", "Typecheck the conflict measurement script")),
    ("test", "scripts/dispatch",
     at("test", "Typecheck the Dispatch census script (bunx tsc --noEmit -p scripts/dispatch)")),
    ("test", "scripts/github-apps",
     at("test", "Typecheck the GitHub App registration script (bunx tsc --noEmit -p scripts/github-apps)")),
    ("test", "scripts/sessions",
     at("test", "Typecheck the session message-wait script (bunx tsc --noEmit -p scripts/sessions)")),
    ("test", "scripts/approval-eval",
     at("test", "Typecheck the approval-eval statistics (bunx tsc --noEmit -p scripts/approval-eval)")),
]

# The trees a filter names file by file rather than whole: check-ts-filters.sh writes into the
# filter a listed block holding one pattern per file under `tree` that the programs PROGRAMS pairs
# with filter `source` compile, in sorted order, and refuses a committed block that is not exactly
# those files. A filter that took the whole tree would select its job for a change to a module no
# program of it reaches; a hand-kept list of the modules would go stale the day one more is
# imported. Derived, it moves with the imports: a new one fails the lint job on the pull request
# that writes it until `.github/scripts/check-ts-filters.sh write` adds it, and a module no longer
# imported is dropped by the same command rather than kept.
#
# Each entry is (filter, tree, source, why):
#   filter  the changes filter whose list holds the block
#   tree    the directory the block lists files of; every other pattern of the filter that matches
#           a file under it is refused, since which of those files the filter selects is the
#           block's to say
#   source  the filter whose PROGRAMS pairs give the files: the filter itself where its job runs
#           those programs, another where the job runs none of its own and loads what that
#           filter's programs compile
#   why     why `source`'s programs are this job's reads of the tree, carried by every refusal
LISTED = [
    ("dispatch_ui", "packages/contracts/src", "dispatch_ui",
     "the dashboard's and the proof editor's typechecks, tests and SPA build compile the contracts modules "
     "these two programs import and what those import, and no other: packages/dispatch/web/vite.config.ts:117-126 "
     "aliases the four entries the dashboard imports to the same sources"),
    # The browser suite has no tsc program of its own: its rows drive the SPA built from
    # packages/dispatch, which bundles the proof editor, and the Playwright config and the e2e
    # helpers it loads are packages/dispatch/e2e, which the dashboard's program compiles. What it
    # loads of @legion/contracts is therefore dispatch_ui's set. Its jobs' `Build contracts for
    # dispatch e2e` step does bundle every entry of the package, packages/contracts/src/legion-api.ts
    # and the modules it imports among them, but no row loads those bundles, and a change that breaks
    # that build fails the contracts job's own `Build contracts` step, whose filter takes the whole tree.
    ("dispatch_browser", "packages/contracts/src", "dispatch_ui",
     "the browser suite runs the SPA built from packages/dispatch and the Playwright config and e2e helpers of "
     "packages/dispatch/e2e, so it loads the contracts modules dispatch_ui's programs compile and no other"),
]

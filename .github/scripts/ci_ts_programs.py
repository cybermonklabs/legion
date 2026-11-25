"""The sources the TypeScript half of each Bun gate's reads is derived from: a filter of ci.yaml's
changes job and either the tsc project the job that filter gates typechecks (PROGRAMS) or the
entry a step of that job runs with Bun (BUNDLES). The changes job's filter generation lists them
(ci_ts_listing.py) into the LISTED blocks, and check-ts-filters.sh, whose header says what it
proves, which gates it leaves out and why, holds every filter to them.

Each entry of PROGRAMS is (filter, project, why):
  filter   a filter of the changes job, the gate of the job whose typecheck this program is
  project  what `bunx tsc -p` takes: the directory holding the tsconfig.json the job's typecheck
           reads, or the project file itself where the job's step names another one
  why      the step the pair stands for, read from that job's own steps and named with the file
           that holds them — ci.yaml's job of that id keeps the gate and calls it — which the
           failure message carries so the reader knows which typecheck holds the file

The pairs are hand-kept and few; the files of each program are derived, which is the point. A
job that typechecks a second project is a second entry (the dispatch-ui job typechecks the proof
editor beside the dashboard, and the tooling-checks job the five script directories that are no
workspace).
"""


def at(job: str, step: str, held_by: str = "") -> str:
    """A `why` naming a step of a ci.yaml job, in the file that holds it: ci.yaml's job of that
    id keeps the gate and calls .github/workflows/ci-job-<id>.yaml, where the steps are, or, for
    the half of a split job, the file of the other half, `held_by` (ci_job_reads.py's
    SHARED_FILE). A job whose steps are in a file of another name writes its own `why`, as the
    docs row does."""
    return f"ci-job-{held_by or job}.yaml's {job} job: {step}"


PROGRAMS = [
    ("contracts", "packages/contracts", at("contracts", "Typecheck contracts (bun run typecheck: bunx tsc --noEmit)")),
    ("envoy_client", "packages/envoy-client", at("envoy-client", "Typecheck Envoy client")),
    ("broker_client", "packages/cipher-client", at("broker-client", "Typecheck the broker client")),
    ("pi_shared", "packages/pi-shared", at("pi-shared", "Typecheck pi-shared")),
    ("pi_envoy", "packages/pi-envoy", at("pi-envoy", "Typecheck pi-envoy")),
    ("pi_legion_build", "packages/pi-legion", at("pi-legion-build", "Typecheck pi-legion", "pi-legion")),
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
    ("tooling_checks", "scripts/dispatch",
     at("tooling-checks", "Typecheck the Dispatch census script (bunx tsc --noEmit -p scripts/dispatch)", "test")),
    ("tooling_checks", "scripts/github-apps",
     at("tooling-checks", "Typecheck the GitHub App registration script (bunx tsc --noEmit -p scripts/github-apps)", "test")),
    ("tooling_checks", "scripts/sessions",
     at("tooling-checks", "Typecheck the session message-wait script (bunx tsc --noEmit -p scripts/sessions)", "test")),
    ("tooling_checks", "scripts/approval-eval",
     at("tooling-checks", "Typecheck the approval-eval statistics (bunx tsc --noEmit -p scripts/approval-eval)", "test")),
    ("tooling_checks", "scripts/e2e/lib",
     at("tooling-checks", "Typecheck the plan ratings read test (bunx tsc --noEmit -p scripts/e2e/lib)", "test")),
]

# The entries a step runs with Bun rather than typechecks: what such a step loads is the entry's
# runtime closure, which no tsc program of its job lists. check-ts-filters.sh builds each one with
# `bun build <entry> --target bun --outdir <scratch> --metafile=<scratch>/meta.json`, whose
# metafile's `inputs` are every module the bundler loaded, resolved under the `bun` export
# condition `bun <entry>` resolves under, keeps the files inside the repository and outside
# node_modules, and holds every one of them to the filter as it holds a program's files. A bundle
# that fails to build, leaves no metafile to read, or leaves one whose `inputs` name no file of the
# repository is a refusal naming the entry, never a bundle of no files.
#
# Each entry is (filter, entry, why):
#   filter  a filter of the changes job, the gate of the step that runs the entry
#   entry   the file `bun build` takes, repository-relative
#   why     the step that runs it and the line of that step's code that does, which every
#           refusal carries
BUNDLES = [
    ("dispatch_two_builds_go", "packages/envoy-client/bin/dispatch.ts",
     at("dispatch-two-builds", "Two-build tests of the Dispatch server (`packages/envoy/cmd/dispatch/twobuild_cli_test.go#currentCLI` runs "
                               "`bun ../../../envoy-client/bin/dispatch.ts`, this tree's dispatch command)")),
]

# The trees a filter names file by file rather than whole: the changes job's generation writes
# into the filter a listed block holding one pattern per file under `tree` that the programs
# PROGRAMS pairs with filters `sources` compile or the bundles BUNDLES pairs with them load, in
# sorted order, and check-ts-filters.sh lists them again in the lint job and refuses a block that
# is not exactly those files. A filter that took the whole tree would select its job for a change
# to a module no program or bundle of it reaches; a hand-kept list of the modules would go stale
# the day one more is imported. Derived when the changes job runs, it moves with the imports.
#
# Each entry is (filter, tree, sources, why):
#   filter   the changes filter whose list holds the block
#   tree     the directory the block lists files of; every other pattern of the filter that matches
#            a file under it is refused, since which of those files the filter selects is the
#            block's to say
#   sources  the filters whose PROGRAMS and BUNDLES pairs give the files, one or more, the block
#            holding the union of theirs: the filter itself where its job runs those programs or
#            that bundle, another where the job runs none of its own and loads what that filter's
#            compile or load
#   why      why `sources`' files are this job's reads of the tree, carried by every refusal
LISTED = [
    ("dispatch_ui", "packages/contracts/src", ("dispatch_ui",),
     "the dashboard's and the proof editor's typechecks, tests and SPA build compile the contracts modules "
     "these two programs import and what those import, and no other: `packages/dispatch/web/vite.config.ts#alias` "
     "aliases the twelve entries the dashboard imports to the same sources"),
    # The browser suite has no tsc program of its own. Its rows drive the SPA built from
    # packages/dispatch, which bundles the proof editor, and the Playwright config and the e2e
    # helpers it loads are packages/dispatch/e2e, which the dashboard's program compiles: that
    # much of @legion/contracts is dispatch_ui's set. Its rows also spawn the dispatch command:
    # `packages/dispatch/e2e/dispatch-cli.ts#dispatchEntry` runs `bun packages/envoy-client/bin/dispatch.ts`
    # for the inbox-cli, asked-of-cli, asked-of and doc specs, and ci_job_reads.py's
    # dispatch_browser gate declares that entry as a read. The contracts modules that command
    # loads are its bundle's, which BUNDLES pairs with dispatch_two_builds_go, so the block is
    # the union of the two sets: a module only the command loads is one the suite loads too.
    # Its jobs' `Build contracts for dispatch e2e` step runs packages/contracts' `build:dispatch`,
    # which bundles the twelve entries the dashboard imports (`packages/dispatch/web/vite.config.ts#alias`
    # aliases the same twelve) and so compiles nothing outside dispatch_ui's set: not
    # packages/contracts/src/legion-api.ts, which neither Dispatch nor the command imports. A
    # change that breaks the package's full `build` fails the contracts job's own
    # `Build contracts` step, whose filter takes the whole tree.
    ("dispatch_browser", "packages/contracts/src", ("dispatch_ui", "dispatch_two_builds_go"),
     "the browser suite runs the SPA built from packages/dispatch and the Playwright config and e2e helpers of "
     "packages/dispatch/e2e, which load the contracts modules dispatch_ui's programs compile, and its rows spawn "
     "the dispatch command (`packages/dispatch/e2e/dispatch-cli.ts#dispatchEntry` runs `bun packages/envoy-client/bin/dispatch.ts`) "
     "whose bundle dispatch_two_builds_go's row builds; no step loads any other"),
    # The browser suite's pull-request lane runs the same file under ci.yaml's dispatch-browser-pr
    # job, so its filter takes the same block for the same reason.
    ("dispatch_browser_pr", "packages/contracts/src", ("dispatch_ui", "dispatch_two_builds_go"),
     "the browser suite's pull-request lane runs the SPA built from packages/dispatch and the Playwright config "
     "and e2e helpers of packages/dispatch/e2e, which load the contracts modules dispatch_ui's programs compile, "
     "and its rows spawn the dispatch command (`packages/dispatch/e2e/dispatch-cli.ts#dispatchEntry` runs "
     "`bun packages/envoy-client/bin/dispatch.ts`) whose bundle dispatch_two_builds_go's row builds; no step loads "
     "any other"),
    # The two-build job's Go step reaches packages/contracts through the one dispatch command its
    # twobuild_cli_test.go runs, which BUNDLES builds.
    ("dispatch_two_builds_go", "packages/contracts/src", ("dispatch_two_builds_go",),
     "the step's cmd/dispatch/twobuild_cli_test.go runs `bun packages/envoy-client/bin/dispatch.ts`, which loads "
     "the contracts modules that entry's bundle loads and no other"),
    # The two-build job runs that Go step and a browser half, which is dispatch_browser's case: it
    # builds the SPA from packages/dispatch and runs the Playwright config and the e2e helpers of
    # packages/dispatch/e2e (`bun run e2e:multiprocess -- cross-build.multiprocess`), and its
    # `Build contracts for dispatch e2e` step runs `build:dispatch`, as dispatch_browser's row
    # says. Its block is the union of the two, so it holds every file the Go step's block holds.
    ("dispatch_two_builds", "packages/contracts/src", ("dispatch_ui", "dispatch_two_builds_go"),
     "the job's browser half runs the SPA built from packages/dispatch and the Playwright config and e2e helpers "
     "of packages/dispatch/e2e, which load the contracts modules dispatch_ui's programs compile, and its Go step "
     "runs the dispatch command whose bundle dispatch_two_builds_go's row builds; no step loads any other"),
]

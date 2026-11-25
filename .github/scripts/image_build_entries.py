"""The image stages that run a Bun build, and what each build bundles: the table
check-image-build-inputs.sh holds each stage's COPY lines to. The script's header says what it
proves and what it leaves out.

Each entry of STAGES is (dockerfile, stage, root, step, builds):
  dockerfile  the Dockerfile, repository-relative; the repository root is its build context, as
              every workflow that builds these images gives it (`context: .`)
  stage       the stage's name, `FROM ... AS <stage>`
  root        where the stage puts the repository: a file `F` of the repository is where the build
              reads it when a COPY of the stage puts it at `<root>/F`
  step        words of the one RUN instruction of the stage that runs the build: the COPY lines of
              the stage above it are what the build can read
  builds      what the step bundles, each one of:
                ("bundle", directory, entry, args): `bun build <entry> <args>` run in `directory`
                    (repository-relative), whose metafile names every module the bundler loaded;
                    `args` repeat the step's own command where they change what it resolves (the
                    target, its conditions, the packages it leaves out)
                ("reported", directory, module, functions): each function of `module`
                    (relative to `directory`), called with a scratch directory, builds into it and
                    returns the absolute path of every file it read; for a build whose entries are
                    the build script's own constant, which a hand-kept list here would go stale on

The steps are few and kept by hand; what each build reads is derived. A new RUN that runs a Bun
build in any Dockerfile and is no step here fails the check until a row names it.
"""

# pi-plugin-prepack.sh, which `bun pm pack` runs, builds the Envoy plugin's bundle
# (`scripts/pi-plugin-prepack.sh#bun build extensions/envoy.ts`), the dispatch command beside it
# (`packages/envoy-client/scripts/build-dispatch-cli.sh#dispatch.js`) and their notices
# (scripts/third-party-notices.ts, run with Bun).
PI_ENVOY_BUILDS = [
    ("bundle", "packages/pi-envoy", "extensions/envoy.ts",
     ["--target", "bun", "--external", "@oh-my-pi/pi-ai", "--external", "@oh-my-pi/pi-coding-agent",
      "--external", "@oh-my-pi/pi-tui", "--external", "@oh-my-pi/pi-utils"]),
    ("bundle", "packages/envoy-client", "bin/dispatch.ts", ["--target", "bun"]),
    ("bundle", ".", "scripts/third-party-notices.ts", ["--target", "bun"]),
]

# The Legion plugin's prepack runs its package's `build` (packages/pi-legion/package.json) and the
# same notices script.
PI_LEGION_BUILDS = [
    ("bundle", "packages/pi-legion", "extensions/legion.ts",
     ["--target", "bun", "--external", "@oh-my-pi/pi-coding-agent", "--external", "@oh-my-pi/pi-tui",
      "--external", "@oh-my-pi/pi-utils"]),
    ("bundle", ".", "scripts/third-party-notices.ts", ["--target", "bun"]),
]

# `bun scripts/build.ts --out` runs the script itself, its bundles (BUNDLE_ENTRYPOINTS, through
# buildBundles) and the Legion plugin it stages (buildLegionPlugin, which reads the skills and the
# agents as files). Both functions return what they read: tests/build-inputs.test.ts reads them
# the same way.
CLAUDE_ENVOY_BUILDS = [
    ("bundle", "packages/claude-envoy", "scripts/build.ts", ["--target", "bun"]),
    ("reported", "packages/claude-envoy", "scripts/build.ts", ["buildBundles", "buildLegionPlugin"]),
]

# The listener image's web stage: the contracts' Go code generator, then Vite's build of the
# dashboard. Vite resolves `@legion/contracts` and the proof editor to their sources
# (`packages/dispatch/web/vite.config.ts#alias`), which the `bun` condition gives Bun too. Each Web
# Worker the page starts with `new Worker(new URL(...))` is a bundle of its own that Bun does not
# follow from the page, so each is a row; so is the config, which Vite loads, with its packages
# left out since `bun install` puts them in place.
WEB_BUILDS = [
    ("bundle", "packages/dispatch", "web/src/main.tsx", ["--target", "browser", "--conditions", "bun"]),
    ("bundle", "packages/dispatch", "web/src/features/refs/markdown-parse.worker.ts",
     ["--target", "browser", "--conditions", "bun"]),
    ("bundle", "packages/dispatch", "web/src/features/refs/mermaid-render.worker.ts",
     ["--target", "browser", "--conditions", "bun"]),
    ("bundle", "packages/dispatch", "web/vite.config.ts", ["--target", "bun", "--packages", "external"]),
]

STAGES = [
    ("packages/daemon/docker/worker.Dockerfile", "plugin", "/repo", "dist/envoy.js", PI_ENVOY_BUILDS),
    ("packages/daemon/docker/worker.Dockerfile", "plugin", "/repo", "dist/legion.js", PI_LEGION_BUILDS),
    ("packages/daemon/docker/worker.Dockerfile", "plugin", "/repo", "scripts/build.ts --out", CLAUDE_ENVOY_BUILDS),
    ("packages/envoy/docker/Dockerfile", "web", "/repo", "bun run gen:go",
     [("bundle", "packages/contracts", "scripts/gen-go.ts", ["--target", "bun"])]),
    ("packages/envoy/docker/Dockerfile", "web", "/repo", "bun run build:web", WEB_BUILDS),
]

# What the withheld capability is, and what it is not

Read it before you unpack, extract or install anything the worker image does not carry.

The withheld `operator-setup` row (`packages/daemon/internal/capabilities/capabilities.go`) reads "installs or configures its own dependencies in the pod: Legion owns its dependencies". It does not forbid the pinned runners of `skill://legion-worker/references/tools-image.md`: the image ships `uv`, `bun` and Go for a role to use (`packages/daemon/docker/worker.Dockerfile`), the `toolchain` row grants them, and the pod puts their caches on the issue's volume (`roleEnvironment` in `packages/daemon/internal/runtime/sandbox/manifest.go`, and `podkind.go`). What it withholds is a dependency the pod carries: something that outlives its command and that a later launch or another role resolves without asking for it.

## The test: where it lands, and what it leaves resolving

An install of your own (an interpreter, a browser, a second toolchain, an extraction) is sanctioned only when both hold:

1. It lands on the issue's volume under `/legion`, outside `$LEGION_WORKSPACE`, `/legion/repos` and `/legion/feed`: under a variable the runtime sets there (`XDG_CACHE_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, `TMPDIR`, `GOCACHE`, `GOMODCACHE`, `BUN_INSTALL_CACHE_DIR`, `UV_CACHE_DIR`, `UV_PYTHON_INSTALL_DIR`), or a path your harness names there. `$HOME` and `/tmp` are on the pod's disk, whose limit evicts the pod. The workspace is excluded because jj snapshots new files there into the change every role shares.
   - Every role of the issue is told the same path under each of those variables but `XDG_STATE_HOME`, which is `/legion/scratch/state/<role>`, a directory named for your role and no shared path.
   - `XDG_CONFIG_HOME` is set too and is not one of them: it is your role's own config home, a copy of the pod's made for your role on the pod's disk, which no other role reads.
2. Once the command exits, nothing else resolves differently. Export a toolchain's `PATH` or `LD_LIBRARY_PATH` only around your harness's own commands, never into the shell you go on working in or a subagent's environment.

- A repository's own dependency install (`uv sync`, `bun install`) into its ignored directories is sanctioned.
- A pinned runner line pins the named package alone; its dependencies are resolved when it runs. When the work needs the whole dependency closure pinned (a comparison that has to hold across days or across pods), use a project's own lockfile with `uv run` or `bun install`, not a one-off runner line.
- Playwright keeps its browsers under `XDG_CACHE_HOME`, on the issue's volume, where the issue's roles share one registry under an install lock. Read the paragraph on sharing the cache home in `docs/kubernetes.md`, "Anatomy of a Sandbox pod", before an install: it says what follows from that sharing. Pointing `PLAYWRIGHT_BROWSERS_PATH` elsewhere loses that shared registry and the lock that makes a second role's install wait rather than race. A tool whose cache the runtime does not place, such as Puppeteer, needs its own variable (`PUPPETEER_CACHE_DIR`) pointed under one of the paths above.
- An install under one of those variables outlives a role's relaunch in its pod. It is not private: every role of the issue can write it. It does not outlive the pod, except on a microVM pod whose deployment keeps the volume (`runtime.kubernetes.sandbox.microvm.keep_volume`). Otherwise the issue's disk is deleted with the pod, so a later pod of the issue starts without it.
- Every cache is on the issue's volume, and the cache trimmer of `skill://legion-worker/references/tools.md` holds part of that volume under a mark. An install is paid once per pod in uv's cache and Pythons, the Go module cache and the Playwright registry, and can be paid again after a sweep in the bun install cache and the Go build cache. Disk is not the ground of the line between a sanctioned install and the withheld one: the volume has the room for every cache (`docs/kubernetes.md`, "The tools a role's own work needs").

## The Dispatch browser suite

A spec you add or change in `packages/dispatch` runs in the pod one spec a command (`bun run e2e -- e2e/<spec file>`), against the pod's own Postgres (*The database your tests use* in `skill://legion-worker/references/tools.md`). A wall-clock spec, whose three `-alone` projects are out of the default selection, runs with a command that names its project: `bun run e2e -- --project=chromium-alone e2e/heavy-bodies.e2e.ts --workers=1` (`packages/dispatch/README.md`, "Projects"). The change's proof is CI's `dispatch-browser` run at the head, which the check that proves the change names as its `ci_check` (`skill://legion-architect/references/design-gate.md#a-check-ci-proves`): an unlabelled pull request runs only the rows tagged `@pr`, and every other row runs under the label `full-ci`.

The local stack (`bun run dispatch:local`, `packages/dispatch/e2e/local-instance.ts`) refuses to start without `go`, `psql`, `flock`, `openssl` and `python3` on `PATH`, and without `docker` too unless a `DATABASE_URL` is given. The image carries all five, and a `DATABASE_URL` stands in for `docker`, so a role can run the local stack, and the specs of the suite its change touches, by file.

One spec needs the same stack as all the specs: a Postgres with pgvector.

- On a digest carrying `legion-testpg`, use the pod's own server and unpack nothing.
- On a digest without it, stand Postgres up from Debian packages unpacked with `dpkg-deb -x` into one root on the issue's volume, as one idempotent script the next role re-runs. The five packages and the start sequence are in `docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md`, and `docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md` puts it on the volume and moves the caches with it. Export the root's `PATH` and `LD_LIBRARY_PATH` around the harness's own commands (`initdb`, `pg_ctl`, the server, the Playwright run), never into the shell you go on working in.
- Hand the harness to the next role as a note, never as an environment it inherits: leave the server running and record its `LD_LIBRARY_PATH`, `PATH` and `DATABASE_URL` in your handoff (an implement handoff's `trickyParts`), so that role sets them deliberately.
- The suite shells out to `psql`, and `psql` is in the image: never extract a client for yourself.

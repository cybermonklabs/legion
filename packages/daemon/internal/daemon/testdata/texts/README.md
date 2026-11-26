# The agents and skills the worker image ships

`shipped-names.txt` lists every agent name and every skill name a real Oh My Pi discovers over both packed plugins, one `agent <name>` or `skill <name>` per line, sorted.
It holds the agents Oh My Pi bundles (`reviewer`, `scout`, `seance`, `security-reviewer`, `sonic` and `task`) beside the eight the plugins ship, and the two hidden `thermonuclear-*` skills beside the fifteen listed ones.
A text may name a bundled agent (`agent="scout"` in `legion-oracle`, `agent="reviewer"` in `simplify-settled-diff`), so a list of the plugins' own files would be short of names the image resolves.
The probe list test of the daemon package (`TestTheProbeListOnTheRealOhMyPi`) holds the load probe's list to these names exactly.

## Capture it again

After a change to a skill, an agent or the Oh My Pi pin, capture the names again:

```sh
OMP=<the Oh My Pi .omp-pin names> bash packages/daemon/internal/daemon/testdata/texts/capture-names.sh
```

`capture-names.sh` packs both plugins from the checkout as the release packs them (`scripts/e2e/lib/pack-plugin.sh`, which needs `bun install --frozen-lockfile` at the repository root), and runs

```sh
omp models --extension <pi-envoy pack> --extension <pi-legion pack> --extension list-names.mjs --json
```

with `LEGION_PROMPT_ROOTS` naming the two packed roots, under a fresh `HOME` and from a fresh working directory, so no user or project skill or agent of the machine enters the list.
That is the boot gate's pod-lane command (`bootgate.go`): `models`, then the three explicit extensions in the order a pod's argv names them, then `--json`.
The flags come after `models`.
`omp --extension <module> models` takes them as the root command's flags and loads none of them for `models`, which then prints no name.
The profile holds one operator provider that nothing listens on, since `omp models` loads its extensions only for a profile with a model, and no run here calls it.

`list-names.mjs` is a stand-alone Oh My Pi extension module.
It makes the two calls `probe.mjs` makes under `LEGION_PROMPT_AGENTS` and `LEGION_PROMPT_SKILLS`: `discoverAgents` with the explicit roots in `explicit-only` mode, and `loadSkills` with the launch's own `skills` settings, `disabledExtensions` and the same roots.
It prints every name found, where the probe prints only an answer about the names it was asked for, and it imports nothing of Legion.
The script keeps only the module's lines, which no other line Oh My Pi prints has the shape of, and checks that `agent scout`, `agent reviewer`, `agent deep-worker`, `skill legion-worker` and `skill dispatch-first` are among them.

## The build

| | |
| --- | --- |
| Pin | `github:cybermonklabs/oh-my-pi@18.18.6-sami.20261125-164517` (`.omp-pin`) |
| Archive | `omp-v18.18.6-sami.20261125-164517-linux-x64.tar.gz`, sha256 `359f95dec797f1a507accbd2f3b56b3ef9b5dfc5fb5cd4c8652a326175ad6c85` (`ARG OMP_SHA256` of `packages/daemon/docker/worker.Dockerfile`), installed as `.github/actions/install-omp/action.yml` installs it |
| `omp --version` | `omp/18.18.6` |
| Binary | sha256 `0e2ddc35dc3d00666b8f7edf5750c32aabcec3e681648442bf55c90a40e18124` |
| Plugins | `@sjawhar/pi-envoy` 16.12.1 and `@sjawhar/pi-legion` 18.14.1, packed from the checkout at 98069553, which holds `main` 22843f79 |

The version string does not tell two builds apart: the Oh My Pi the worker image carries prints the same `omp/18.18.6` and is another binary.
The file holds 14 agents and 17 skills.

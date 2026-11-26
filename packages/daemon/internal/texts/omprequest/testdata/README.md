# First requests of a real Oh My Pi

`requests/` holds the first request body a real Oh My Pi sent a model stand-in, in each of three cases.
`omprequest.Read` reads them, and `TestTheReaderReadsEachCapturedRequest` holds the reader and each case's facts to these bytes.
Every test that asks what a launch of a folder of texts offers its model reads a request the same way.

## The command that captured them

```sh
OMP=<the Oh My Pi .omp-pin names> bash packages/daemon/internal/texts/omprequest/testdata/capture.sh
```

`capture.sh` packs both plugins from the checkout as the release packs them (`scripts/e2e/lib/pack-plugin.sh`), makes the roots below, and starts a python3 stand-in that records each POST body and answers every one with one text turn.
It then runs, once for each case:

```sh
omp --mode rpc --extension <root> --extension <pi-envoy pack> --extension <pi-legion pack> \
    --append-system-prompt <file> --no-lsp --no-title
```

with `DISPATCH_URL` set (so pi-envoy adds the dispatch-first skill to the request), under a fresh `HOME` and from a fresh working directory, both made with `mktemp -d`, so no user or project skill of the machine enters a fixture.
`--no-lsp` and `--no-title` keep out a language-server start and a title request, which have nothing to do with the question.
The run needs `bun install --frozen-lockfile` at the repository root for the pack.
`capture.sh` removes its scratch directory when it ends.

A fixture is the first request that carries a system prompt and tools, formatted by the repository's Biome (`capture.sh` runs it last, since lint holds every committed JSON file to it), with the fresh working directory's path replaced by `/work/cwd`.
Nothing else is changed.
The stand-in's address is on loopback and appears in no fixture.

## The build

| | |
| --- | --- |
| Pin | `github:cybermonklabs/oh-my-pi@18.18.6-sami.20261125-164517` (`.omp-pin`) |
| Archive | `omp-v18.18.6-sami.20261125-164517-linux-x64.tar.gz`, sha256 `359f95dec797f1a507accbd2f3b56b3ef9b5dfc5fb5cd4c8652a326175ad6c85` (`ARG OMP_SHA256` of `packages/daemon/docker/worker.Dockerfile`), installed as `.github/actions/install-omp/action.yml` installs it |
| `omp --version` | `omp/18.18.6` |
| Binary | sha256 `0e2ddc35dc3d00666b8f7edf5750c32aabcec3e681648442bf55c90a40e18124` |
| Plugins | `@sjawhar/pi-envoy` 16.12.1 and `@sjawhar/pi-legion` 18.14.1, packed from the checkout at 98069553, which holds `main` 22843f79 |

The version string does not tell two builds apart: the Oh My Pi the worker image carries prints the same `omp/18.18.6` and is another binary.
Capture again with the build `.omp-pin` names, and update this table.

## The three cases

Each root is an `--extension` directory holding `agents/` and `skills/` and nothing else: no `package.json`, no extension entry.
The marker `Changed by the arm.` heads the description of each text the arm changes, since a request shows only a description.

### 1. `1-changed-skill.json`

- The root: a `legion-worker` skill with a changed `SKILL.md`, and a `goal-red-team` agent with a changed description.

- The settings layer: none.

- The request lists 16 skills.
  The plain `legion-worker` is the root's changed copy, and the plugin's differing copy is listed as the alias `pi-legion/legion-worker`.

- `goal-red-team` heads the `task` tool's agent list, once, changed.

### 2. `2-changed-skill-hidden.json`

- The root: the first case's.

- The settings layer: `PI_CONFIG_FILES` names a layer that sets `skills.ignoredSkills` to `*/legion-worker` and `*/legion-worker~*`.

- The request lists 15 skills.
  The plain `legion-worker` is the root's changed copy, and there is no alias.

- The agent list is the first case's.

### 3. `3-changed-reference.json`

- The root: a `legion-worker` skill whose only change is `references/plan-tests.md`, its `SKILL.md` the plugin's own bytes.

- The settings layer: none.

- The request lists 15 skills.
  `legion-worker` is listed once, unchanged: the identical `SKILL.md` collapsed, and there is no alias.

- The agent list holds no changed agent.

The 15 skills of the second and third cases are `agent-secrets`, `approval-review`, `asd-ste100`, `dispatch`, `dispatch-first`, `dispatch-writing-specs`, `envoy`, `legion-architect`, `legion-controller`, `legion-oracle`, `legion-retro`, `legion-worker`, `outside-message`, `plain-words` and `simplify-settled-diff`.
The first case adds `pi-legion/legion-worker`.
The two `thermonuclear-*` skills the plugins ship are hidden from the list, as their `disable-model-invocation` frontmatter asks.

The `task` tool lists the plugins' eight agents and the six Oh My Pi bundles, `reviewer`, `scout`, `seance`, `security-reviewer`, `sonic` and `task`, each once.

### The namespace

The namespace Oh My Pi gave the plugin's copy is `pi-legion`, so an alias holds exactly one `/`: `pi-legion/legion-worker`.
It is the name of the directory the plugin's skills came from (the unpacked pack is `pi-legion`, as `/opt/legion/pi-legion` is in the worker image).
Measured beside the fixtures, and not kept: a copy of the plugin in a directory named `zz-plugin-dir` gave `zz-plugin-dir/legion-worker`, and the plugin linked into a profile with `omp plugin install` gave `pi-legion/legion-worker` too.
The patterns `*/legion-worker` and `*/legion-worker~*` of the second case matched the alias and removed it.
No case makes the `~2` form, which Oh My Pi gives a namespaced name that is itself taken.

## The measurement: does a root of only `agents/` and `skills/` load as an `--extension` root?

Yes.
Oh My Pi read the directory with no `package.json` and no extension entry, and the request shows both halves of it.
Its skill is listed under the plain name (the plugin's differing copy took the alias, which it would not have if the root's were absent), and its agent heads the `task` tool's list with the changed description.
Oh My Pi printed no error for the root: its stderr held at most a notice that startup was slow (`Still starting after 10s`), which a busy machine causes.
The root therefore needs no `package.json` with `"omp": {"skills": ["skills"]}`, and the captures above are the first ones, taken with none.

The root came first in the command line and its skill kept the plain name.
Oh My Pi's documentation says the root it admits first keeps the plain name of a skill that differs, which is why a launch puts the root first.
No capture here put the root after the plugins.
Measured beside the fixtures, and not kept: with the plugins linked into the profile by `omp plugin install` and only the root named by `--extension`, the first case's request listed the same skills and the same agents in the same order.

## What else the capture showed

- With a settings layer named in `PI_CONFIG_FILES`, Oh My Pi first sends one skill-description compression request for each skill (a request with no system prompt, which starts `Compress into one routing hint`), then the conversation's.
  With no layer it sends none.
  A layer that sets only `skills.enabled`, or only an `ignoredSkills` entry matching no skill, did it too.
  `capture.sh` skips those requests.

- The first conversation request is followed by a side request (`<btw>`), which the stand-in also answers.
  It is not a fixture.

- The dispatch-first skill is the first user message: a `<system-reminder>` with the date and the working directory, then the skill between `<dispatch-first-skill>` and `</dispatch-first-skill>`.

- The skill list is in the system prompt between `<skills>` and `</skills>`, one `- <name>: <description>` line each, descriptions cut at about a hundred characters.
  The agent list is in the `task` tool's description under `# Available Agents`, one item each, which starts `- ` and the name in backticks.

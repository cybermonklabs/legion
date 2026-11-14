# r7: the Legion phase workflow written in MAS Lab's specification format, loaded by MAS Lab's own loader

Input: packages/daemon/internal/workflow/table.go at 9542c6fbbecd184ccee73523ab3defa39d6cf23a, the thirty-eight rows of var Table.

Requirement r7 asks whether MAS Lab's specification format can hold work we already have.
I took the one table that defines how a Legion issue moves, wrote it in their format, and loaded it with their loader.
The specification I wrote is `evidence/legion-workflow.yaml`; a second spelling of the same workflow, in their other format, is `evidence/legion-workflow-kind.yaml`; every command and every output is `evidence/maslab-load.txt`, produced by `evidence/tm-load.sh`.

## What one row of ours holds

`Row` has five fields: `From`, `Trigger`, `Guard`, `To` and `Effects` [tm-our-row].

`Guard` is a Go function value, `func(Snapshot) bool`, and `Snapshot` is the record state the daemon builds: `Phase`, `HasOpenOrMergedPR`, `GateOpen` [tm-our-guard].

`Effects` is a list of `EffectKind`: `suspend`, `start`, `notice`, `linger`, which the comment above the type calls "the durable outbox consequences selected by a row" [tm-our-effects].

`Table` holds 38 rows: 17 written out in the composite literal and 21 that `backwardRows` builds from `phase.BackwardPairs` [tm-our-count].

I counted them by running the table through a test injected with `go test -overlay`, so our tree stayed unchanged; the test printed `rows=38 literal=17 backward=21` [tm-our-count].

The two files of ours this evidence reads, `table.go` and `phase.go`, are byte for byte the same at the pinned commit 9542c6f and in the working tree I read, whose head is bfe1471 [tm-our-bytes].

## Their format, and what loaded

MAS Lab's format for a thing like this is a `kind: Agent` manifest whose `spec.design_pattern` names the `state_machine` plugin, with the state table under `params` [tm-format].

A state table is `start`, `vars`, `states` and `signals`; a state names a `run` kind and a list `on` of transitions; a transition holds `when`, `set` and `goto`, and nothing else [tm-transition-keys].

Their installer path works: mas-lab is a `uv` workspace, and `uv pip install ./runtime ./ctl ./library-standard` from the clone at 477441d installs mas-lab-runtime, mas-lab-ctl and mas-lab-library-standard 0.2.0 [tm-install].

The brief's premise that mas-lab is not on PyPI is false today: `https://pypi.org/pypi/mas-lab/json` answers 200 with version 0.2.0, one release, and the files `mas_lab-0.2.0-py3-none-any.whl` and `mas_lab-0.2.0.tar.gz` [tm-pypi].

Their table loader takes the workflow: `normalize_table` followed by `validate_definition` returns an empty problem list, and the loaded table holds 10 states and 38 transitions, one for each row of ours [tm-table-loads].

Their manifest validator takes both files: `mas-ctl validate legion-workflow.yaml legion-workflow-kind.yaml` prints `OK` for each and exits 0 [tm-validate].

Their spec parser takes the manifest: `parse_agent_spec` returns a `KernelConfig` whose `pattern_plugin_id` is `state_machine` and whose `max_auto_steps` is the 200 I wrote [tm-parse].

Their compile-only path keeps the whole table: `mas-ctl compile` writes a resolved manifest with 38 `goto:` lines [tm-compile].

The plugin's own load of the table passes when the seven agents the table names are participants of the run, and refuses with `state_machine: ['architect', 'daemon', 'implementer', 'merger', 'planner', 'reviewer', 'tester'] are not agents this entry can delegate to ([]).` when they are not [tm-configure].

A run refuses without a model key: `mas-ctl chat` exits 1 with `error: Live LLM configured but API key env unset: OPENAI_API_KEY (leaf openai-direct).` [tm-run-refusal].

So everything below the model call is checked here, and nothing about how a run behaves is [tm-run-refusal].

## What their format cannot hold

Each line names the part of our workflow, what of theirs I looked for, and the search that found nothing.

The trigger has no field.
`TRANSITION_KEYS` is `{when, set, goto}`, and `grep -rnE "trigger"` over their `state_machine.py` is empty.
The nearest thing of theirs is `signals`, transitions matched against a steer message's `signal.text`, `signal.sender`, `signal.source` and `signal.intent`.
I had to write each of our 13 triggers as a field of the agent's reply, which moves the authorship of the trigger from the daemon's own observation to the worker's answer [tm-gap-trigger].

The guard cannot be a function.
`when` is a mapping of a scope path to one of nine data operators, `eq ne in not_in gt gte lt lte exists`, and `grep -rnE "eval\("` over their `state_machine.py` is empty, so there is no expression and no callable either.
Our one data guard, `s.HasOpenOrMergedPR`, survives as `when: {out.has_open_or_merged_pr: {eq: true}}`; a guard that is any other Go code has no spelling at all [tm-gap-guard].

The guard's input is not reachable.
Our guard reads a `workflow.Snapshot` the daemon builds from its records.
Their scope, `_scope`, is the run's own variables plus `task`, `steps`, `state`, `elapsed`, `transcript`, `out`, `last` and `signal`; no entry of it comes from a record store.
I had to ask the agent for `has_open_or_merged_pr` in its reply, so the guard now trusts the worker about the state of its own pull request [tm-gap-guard-input].

The effects have no field.
A transition carries `set` and `goto` alone, `grep -rnE "effect"` over their `state_machine.py` is empty, and `grep -rni outbox --include=*.py` over the whole clone is empty.
The `set: {effects: start,notice}` lines in my file write the effect words into a variable; nothing in their engine reads that variable, so they are a note to the reader, not an effect [tm-gap-effects].

Nothing is durable or transactional.
Their module says the run state "lives on the plugin for the turn", and `grep -rnE "durable|persist"` and `grep -rnE "transaction|atomic|rollback"` over their `state_machine.py` are both empty.
Our engine applies a row's effects and its phase write in one transaction [tm-gap-durable].

A phase that waits for an outside event has no run kind.
`RUN_KINDS` is `delegate, react, pattern, parallel, tool, end`, and `grep -rnE "'wait'"` over their `state_machine.py` is empty; every state that is not `end` runs a child, and a transition is tried only at that child's boundary.
Our `admitted` waits for the controller's slot gate and our `awaiting_merge` waits for GitHub, so I wrote both as a delegation to an agent named `daemon` that is asked what happened [tm-gap-wait].

The status a row writes has no field.
Our table's `PhaseStatus` gives each target phase the Dispatch status an issue shows while it stands there, and the same search that found no effect key found no status key [tm-gap-status].

The 21 backward rows cannot be generated.
Ours come from `phase.BackwardPairs`, the one list the config loader reads too, and a test holds the two equal.
A MAS Lab table is data in a manifest, so I wrote the 21 transitions out by hand, and nothing ties them to a list or notices when one is missing [tm-gap-backward].

No coverage is checked.
`validate_definition` checks shape: that `start` is a state, that some state is `run: end`, that each `goto` names a state, that keys are known.
It cannot check that every phase has a row for every trigger, because it has no idea of a trigger; our `validateTable` and `TestDeletingAnyTransitionRowBreaksCoverage` do exactly that [tm-gap-coverage].

## Their other format, the one called workflow

They also have `kind: Workflow`, `apiVersion: workflow/v1`, whose spec is `entry`, `nodes` and `edges`, and an edge holds `from`, `to`, `condition` and `label` with `additionalProperties: false` [tm-wf-kind].

A node of that schema is an agent, "Node ID — must match the agent metadata.name declared in the MAS manifest", so a phase is not what a node means [tm-wf-node].

Its `condition` is described as an "Optional routing condition (Python-like expression evaluated at runtime)", which is a claim of the project about its own schema, not evidence of code [tm-wf-quote].

Nothing reads it: `grep -rn "class WorkflowSpec" --include=*.py` over the clone is empty, although the schema's own header says its field list "is derived from WorkflowSpec in workflow.py", and `grep -rnE "\"edges\"|'edges'" --include=*.py` over `runtime/src` and `ctl/src` is empty as well [tm-wf-noreader].

Their validator still passes the file: `mas-ctl validate legion-workflow-kind.yaml` prints `OK ... kind=workflow`, so a document in this kind is validated by a schema and run by nothing [tm-wf-validated].

## Two findings about the validator itself

From a plain wheel install of mas-lab-ctl 0.2.0 the schemas are missing, and `mas-ctl validate` prints `OK legion-workflow.yaml kind=agent` with the warning `no schema file for kind 'agent'` and exits 0, so an `OK` from an installed mas-ctl can mean nothing was checked [tm-schema-wheel].

From a source checkout install of mas-lab-ctl alone the schemas are found and the same command crashes with `ModuleNotFoundError: No module named 'mas.lab'`, because the validator imports `mas.lab.schemas.validate` and mas-lab-component-core is not a dependency of mas-lab-ctl [tm-schema-crash].

## One hazard of the format

Their state key `on` is the YAML 1.1 boolean `True` after `yaml.safe_load`, in all nine of my states, which is why their `normalize_table` exists; and `mas-ctl compile` writes the compiled manifest back with nine `true:` keys rather than `on:` [tm-yaml-on].

## Re-running this

`evidence/tm-load.sh` is the script that ran every command above in order, from the clones at their pinned commits, and wrote `evidence/maslab-load.txt`; it names the paths of the pod that ran it, so it is a record rather than a command to run again.
The rows `tm-*` of `claims.jsonl` carry the same commands, and `bash docs/research/internet-of-cognition/check.sh --replay` runs each of them again on any machine, under the environment `env.sh` prepares.

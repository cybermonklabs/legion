# mas-lab `library-eval/`

Subject: the directory `library-eval/` of the repository `mas-lab` of github.com/ioc-project.

Clone commit: 477441ddb82d1b07c76c36f1426d8a5be0b6194b, in `$IOC_CLONES/mas-lab`, where `IOC_CLONES` is `/legion/scratch/tmp/ioc-clones`.

Our own tree is read at commit 9542c6fbbecd184ccee73523ab3defa39d6cf23a.

Requirement: r3.

## What the library is

`library-eval` scores one agent run with quality metrics [m-eval-purpose].

It wraps the Metrics Computation Engine (MCE), a set of LLM-as-judge metrics of the AGNTCY `telemetry-hub` project [m-eval-purpose].

It gives two surfaces: the command `mas-lab eval`, and the pipeline step `eval_mce` [m-eval-surfaces].

The step itself is not in this directory: it is in `library-lab`, and it calls this library [m-eval-surfaces].

## What it measures

The catalog holds 11 session metrics [m-eval-stock-11].

Each one is a prompt to a judge model: `goal_success_rate`, `groundedness`, `response_completeness` and eight more [m-eval-stock-11].

A stock metric reads the input and the output of the run, not the event history [m-eval-stock-io].

Six more metrics are rule-based and count tool errors, cycles and agent-to-agent calls [m-eval-core-rule].

A user can add two kinds of metric of his own.

The first is an inline metric: an id, a prompt, a unit and an evidence kind, written in the step's YAML [m-eval-prompt-metric].

The unit is `mas`, `agent` or `call`; the evidence is `io` or `trajectory` [m-eval-prompt-metric].

The second is a plugin class, declared in a `library.yaml` and registered on the provider [m-eval-plugin-metric].

The judge must answer with a JSON object that holds a number, or the word YES or NO [m-eval-judge-json].

The library does not own the text of a stock metric's prompt: that text is in MCE [m-eval-prompt-owner].

## What it needs to run

It needs one trace file for each run: `events.jsonl`, in the shape the mas-lab event recorder writes [m-eval-input-trace].

The reader of that file names nine event kinds and five event fields [m-eval-trace-kinds].

It needs a judge model and an OpenAI-compatible endpoint [m-eval-needs-key].

The key comes from an environment variable; when no variable holds a key, the client is built with the string `none` and the failure comes later, at the call [m-eval-needs-key].

It needs no database and no message broker [m-eval-no-service].

It needs a Python 3.11 toolchain and eight packages: `mas-lab-component-core`, `mas-lab-component-bench`, `mas-lab-runtime`, `metrics-computation-engine`, `mce-metrics-plugin`, `deepeval`, `click` and `pandas` [m-eval-deps].

## What it writes

It writes one `metrics.json` for each run [m-eval-writes].

That document holds `schema_version`, `item_id`, `scenario`, `session`, `run_quality` and `computed_at` [m-eval-writes].

Each metric id in `session` holds a value, the judge's reasoning, an error, and optional details [m-eval-score-shape].

A metric that fails writes a null value and an error for that id alone [m-eval-score-shape].

## What an experiment is

An experiment is a matrix: scenarios, times dataset items, times runs [m-eval-experiment-unit].

A scenario is a stack of overlays over one MAS manifest [m-eval-scenario-overlay].

An overlay is a patch of the agent manifest, so a changed prompt or a changed pattern is one overlay [m-eval-overlay-is-patch].

A dataset is a manifest of kind `Dataset` whose `spec.items` hold the inputs [m-eval-dataset-kind].

The run count is `run.n_runs`, and its default is 3 [m-eval-n-runs].

The judge model is a slot on the experiment: `models.judge`, or `evaluation.model`, or a per-step `config.model` [m-eval-judge-slot].

The runner executes the MAS through the mas-lab runtime [m-eval-runner-is-mas].

## What the stack does with the numbers

A step aggregates the scores of N runs into a mean, a standard deviation and a confidence interval, by Student's t, by bootstrap or by the normal approximation [m-eval-compute-ci].

That step can collapse the runs of one item first, so that the item and not the run is the unit [m-eval-compute-ci].

The older analysis class gives a mean and a standard deviation for each pattern, and a pivot table of means for a comparison of patterns [m-eval-analysis-means].

No part of the repository runs a test of the difference between two arms [m-eval-no-difference-test].

## Question 2: could its experiment harness measure whether a change to a role's prompt or skill made Legion better?

The scoring part could, after work [m-eval-answer].
The running part could not [m-eval-answer].

The reason is the unit.
The harness runs a MAS that the mas-lab runtime instantiates [m-eval-runner-is-mas].
A Legion role is an Oh My Pi session that our own rig starts from a packed plugin of a checkout [m-eval-our-rig].
The harness has no way to start that session, so only the file-scoring surface, `mas-lab eval SOURCE`, is usable for us [m-eval-surfaces].

These are the five things we would have to supply.

**The unit under test.**
One directory for each run, holding `traces/events.jsonl` in the mas-lab shape [m-eval-input-trace].
We hold Oh My Pi session JSONL, not that shape, so we would write a converter for the nine event kinds its reader names [m-eval-trace-kinds].
The step also expects the tree `item*/r*/traces/events.jsonl`, which fixes our run directory layout [m-eval-runs-dir-tree].

**The metric.**
No stock metric asks a question about a Legion role.
The 11 stock ids score the input and the output of the whole run [m-eval-stock-io].
What a Legion role does well or badly is in the process: which ask it opened, which file it read first, what its handoff recorded.
We would write the metric as an inline prompt metric with `unit: mas` and `evidence: trajectory` [m-eval-prompt-metric], or as a plugin class [m-eval-plugin-metric].
Our rig already states six such rules as deterministic code [m-eval-our-scorers].

**The dataset.**
A `Dataset` manifest whose items are the tasks the role gets [m-eval-dataset-kind].
We hold seven scenarios already [m-eval-our-scenarios].
Each one is a seeded world, with a Dispatch, a daemon stand-in and a bare remote, and not a line of text [m-eval-our-rig].
A dataset item holds an id, a set of inputs and a category, and holds no world [m-eval-dataset-kind].

**The number of runs.**
The declaration is `run.n_runs`, and its default is 3 [m-eval-n-runs].
Our own documented comparison uses 5 runs for each label [m-eval-our-n5].
The aggregation step gives a confidence interval over those runs [m-eval-compute-ci], which is more than our rig prints today [m-eval-our-no-stats].
It still gives no test of the difference between two arms [m-eval-no-difference-test].
The decision rule stays ours to write.

**The judge.**
A model id and an OpenAI-compatible endpoint with a key [m-eval-needs-key].
The slot is `evaluation.model` or `models.judge` [m-eval-judge-slot].
We run judges already: ten judge prompts, nine of them on each spec run [m-eval-our-judge-prompts], each in a fresh container [m-eval-our-judge].

One more thing is needed that the list above hides: a control.
A comparison of two labels measures the rule only when a label whose skills lack the rule scores lower.
Our rig states that rule and requires an ablate arm for every comparison [m-eval-ablate-rule].
mas-lab names the ablation as a use of the harness and shows a `baseline` scenario with no overlays, but it is a convention of the manifest and no code holds it [m-eval-control-convention].

## What of ours exists for this today

Our tree already measures a skill change on real agents [m-eval-our-rig].

`rig.sh profile <label> <checkout>` builds one isolated agent home for each version [m-eval-our-rig].

`rig.sh batch <scenario> <n> <label>...` then runs n agents for each label [m-eval-our-rig].

Seven scenarios exist [m-eval-our-scenarios].

One scorer scores every label, from what the stand-ins and the scratch Dispatch recorded, never from what the agent said [m-eval-our-scorers].

`score.ts` holds six scenario rules [m-eval-our-scorers].

`judge.ts` runs ten judge prompts on the spec scenarios [m-eval-our-judge-prompts].

The rig is 18144 lines [m-eval-our-rig-size].

Three things of ours are missing.

There is no MCE and no deepeval anywhere in our tree [m-eval-our-no-mce].

There is no confidence interval and no significance number in the rig [m-eval-our-no-stats].

No continuous-integration job runs a batch, so an operator runs the rig [m-eval-our-rig-not-ci].

## Cost

The smallest honest version for Legion is this.
Convert one scenario's transcripts to the mas-lab trace shape, score them with one inline prompt metric, aggregate the runs into a confidence interval, and compare two labels.

The estimate is 7 engineer-days for that route [m-eval-cost].

The same measurement on the rig we already have costs 2 engineer-days, because only the decision rule is missing [m-eval-cost].

The estimate rests on five counted things.

The converter must emit nine event kinds [m-eval-trace-kinds].

The eight Python packages are a new toolchain in a repository of TypeScript and Go [m-eval-deps].

Six scoring rules are already written and would have to be stated again as prompts [m-eval-our-scorers].

The rig is 18144 lines, which shows that the cost of this work is the seeded world and not the scorer [m-eval-our-rig-size].

The documented comparison uses 5 runs for each label [m-eval-our-n5].

The estimate itself is a judgement and not a measurement [m-eval-cost].

No trial conversion was run, and this tree holds no record of the engineer-days a comparable converter took, so the number is not externally checkable [m-eval-cost].

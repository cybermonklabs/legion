# ACCORD's term similarity, read and re-run

Requirement r1.
Clone `cognition-protocols` at `e3c7bf10f676de37ac08d2fe97e7de982f74629a`.
Our tree at `9542c6fbbecd184ccee73523ab3defa39d6cf23a`.
`$IOC_CLONES` is `/legion/scratch/tmp/ioc-clones`.

## What the scorer is

ACCORD scores two definitions of one term with the class `Similarity` in
`protocols/accord/skills/accord-initiator/scripts/accord_initiator.py`, lines 96 to 139 [s-scorer-two-backends].
It has two backends.
When `sentence_transformers` imports and the named model is already on disk it embeds both texts and takes the cosine [s-scorer-model-backend].
Otherwise it returns `difflib.SequenceMatcher(None, na, nb).ratio()` over the lower-cased alphanumeric tokens of the two texts [s-scorer-difflib-backend].

The model is loaded with `local_files_only=True` (line 113), so a machine with the library but no cached model never fetches one and always takes the difflib branch [s-scorer-local-only].
The default model name is `all-MiniLM-L6-v2` (line 59) [s-scorer-model-name].
The environment variable `ACCORD_EMBEDDING` set to `0`, `off`, `false` or `none` turns the model backend off (line 105) [s-scorer-env-switch].

The fallback is silent.
The import and the load sit in a `try` whose handler is `except Exception:` at lines 114 and 115, and the handler only sets `self._embedder = None` [s-scorer-silent-fallback].
The clause names `Exception`; it is not a bare `except`, which would also swallow `KeyboardInterrupt` [s-scorer-except-clause].
Nothing in the file writes a log line, a warning or a stderr line about the choice, and no output of the tool names the backend that produced a score [s-scorer-no-backend-record].

The design record under check also cites `accord_mce_metrics.terms.load_scorer` as this scorer.
That function has one backend, not two: it returns `string_similarity`, the difflib ratio, for every call, and no branch of it can return a model (`terms.py`, lines 86 to 88) [s-loadscorer-one-backend].
The whole `mce-metrics` package names `sentence_transformers` nowhere [s-mce-no-model].
So ACCORD's own measurement package scores term agreement with difflib alone, whatever the kernel does [s-mce-difflib-only].

## The thresholds

The design record states 0.72 and 0.55, and both hold.

`SIM_MERGE = 0.72` and `GROUP_THRESHOLD = 0.72` at lines 62 and 61 [s-threshold-merge].
`SIM_CLARIFY = 0.55` at line 67 [s-threshold-clarify].
`terms.SIM_MERGE = 0.72` at line 20 of `terms.py` [s-threshold-metrics].

The bands give three verdict words, and this table uses them.
A pair at or above 0.72 joins one cluster, and a term whose units all fall in one cluster is written `agreed` (lines 336 and 341 to 344) [s-verdict-agreed].
A cross-cluster pair in `[0.55, 0.72)` is written `ambiguous` and sent to CLARIFY (lines 356 to 362); this table calls that verdict `clarify` [s-verdict-clarify].
A term with more than one cluster is written `flagged` (lines 346 to 350), and a pair below 0.55 reaches no other verdict, so this table calls that verdict `flagged` [s-verdict-flagged].

## The model that was run

The model is `sentence-transformers/all-MiniLM-L6-v2`, the family ACCORD names, at revision `1110a243fdf4706b3f48f1d95db1a4f5529b4d41`.
Its `model.safetensors` is 90868376 bytes and its sha256 is `53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db` [s-model-checksum].
It is fetched under the cache directory `docs/research/internet-of-cognition/env.sh` prepares, never the home directory [s-model-install].
The run used `sentence-transformers` 6.3.0 on `torch` 2.15.0+cu132 and CPython 3.12.15 [s-model-versions].

Setup, which every recorded command below assumes: `docs/research/internet-of-cognition/env.sh` builds it and exports what the commands read.

```sh
. docs/research/internet-of-cognition/env.sh
```

It makes the virtual environment, installs `sentence-transformers`, fetches that model snapshot and checks its `model.safetensors` against the checksum above, and exports `IOC_PY`, `IOC_MODEL_DIR` and `ACCORD_SRC`.
`check.sh --replay` sources it before it runs the first recorded command, so a reader runs nothing by hand.

The run itself:

```sh
cd docs/research/internet-of-cognition/similarity && ACCORD_MODEL_DIR="$IOC_MODEL_DIR" "$IOC_PY" similarity.py
```

The script is `docs/research/internet-of-cognition/similarity/similarity.py` and its output is `evidence/similarity.txt`.
It imports ACCORD's own `Similarity` class from the clone and calls it; it re-implements no scoring [s-run-uses-their-code].
Each backend scores in its own pass, because the class reads `ACCORD_EMBEDDING` at call time, and the difflib pass asserts that no embedder loaded.
The script also asserts, for every pair, that the difflib score equals `accord_mce_metrics.terms.string_similarity`, so one number covers both of ACCORD's difflib paths [s-run-crosscheck].

## The nine pairs

Five pairs are the ones this issue named.
Four are the reader's choice from our own record, each named with where it came from, and they are a choice and not a sample: no frame of our term disagreements was enumerated and no draw was made from one [s-pairs-chosen].
Their texts are short restatements of two readings our own files record, not quotations [s-pairs-ours].

| pair | the truth | difflib score | difflib verdict | model score | model verdict | the pair |
| --- | --- | --- | --- | --- | --- | --- |
| p1 | differs by 100x | 0.923 | agreed | 0.794 | agreed | `fraction 0-1` / `fraction 0-100` |
| p2 | differs by 1000x | 0.898 | agreed | 0.937 | agreed | `p99 latency in milliseconds` / `p99 latency in seconds` |
| p3 | inverted | 0.902 | agreed | 0.944 | agreed | `severity 1 is most severe` / `severity 1 is least severe` |
| p4 | different acts | 0.580 | clarify | 0.757 | agreed | `approved means a person approved this version` / `approved means CI passed` |
| p5 | the same, said twice | 0.467 | flagged | 0.950 | agreed | `the deploy is the merge to main` / `a merge to main is the deploy` |
| p6 | different events | 0.458 | flagged | 0.660 | clarify | `done means the change is driven in production` / `done means the issue's last workflow phase finished` |
| p7 | inverted | 0.782 | agreed | 0.907 | agreed | `unresponsive means the session has not answered yet` / `unresponsive means the session ended` |
| p8 | inverted | 0.786 | agreed | 0.853 | agreed | `a parked child is stopped, not done` / `a parked child is done` |
| p9 | inverted | 0.412 | flagged | 0.481 | flagged | `from is the sender's own claim` / `from is proof of who wrote the message` |

Where the four chosen pairs come from, in our tree at `9542c6fbbecd184ccee73523ab3defa39d6cf23a`:

p6 is `done` driven in production (`skills/dispatch/SKILL.md:45`) against `done` as the workflow phase a root reaches while its status is still `in_progress` (`skills/legion-controller/SKILL.md:50-51`) [s-pair-p6-source].

p7 is the two readings of `unresponsive` that `skills/envoy/SKILL.md:49` sets against each other: "it has not answered yet", not "it ended" [s-pair-p7-source].

p8 is the two readings of a parked child that `skills/legion-architect/SKILL.md:45` sets against each other: "a parked child is stopped, not done" [s-pair-p8-source].

p9 is the two readings of a message's `from` that `skills/envoy/SKILL.md:35` sets against each other: the sender's own claim, and never proof of who wrote the message [s-pair-p9-source].

Each row of the table is two claim rows, the difflib score against the threshold it crosses and the model verdict word.

p1 [s-row-p1-difflib, s-row-p1-model].

p2 [s-row-p2-difflib, s-row-p2-model].

p3 [s-row-p3-difflib, s-row-p3-model].

p4 [s-row-p4-difflib, s-row-p4-model].

p5 [s-row-p5-difflib, s-row-p5-model].

p6 [s-row-p6-difflib, s-row-p6-model].

p7 [s-row-p7-difflib, s-row-p7-model].

p8 [s-row-p8-difflib, s-row-p8-model].

p9 [s-row-p9-difflib, s-row-p9-model].

## What the table shows

Both backends merge pairs that mean different things.
Of the eight pairs that are not the same, difflib called five `agreed`, which is ACCORD's word for one shared definition [s-sum-difflib].
The model called six of those eight `agreed` [s-sum-model].
Every inverted pair except p9 scored above the merge threshold on both backends: p3, the severity inversion, scored 0.902 on difflib and 0.944 on the model [s-row-p3-difflib, s-row-p3-model].
The one pair that really is the same, p5, is the one difflib refused: it scored 0.467 and was `flagged`, while the model scored 0.950 and called it `agreed` [s-row-p5-difflib, s-row-p5-model].

So the backend chosen changes the verdict, and the choice is made silently.
On the nine pairs the two backends disagreed on the verdict word for p4, p5 and p6 [s-backends-disagree].
A reader of ACCORD's output cannot tell which backend produced a number, because no output names it [s-scorer-no-backend-record].

Per-row evidence rows carry a predicate, not a sentence, because the model run is not bit-identical across machines: each difflib row carries a bound on the score against the threshold it crosses, and each model row carries the set the verdict word must fall in.

## Scope and limits

The model column is one run of one model on one machine.
Another `all-MiniLM-L6-v2` build may move a score by a small amount; the recorded predicates are the bound each row needs, not the exact number.
The verdict words are this table's rendering of ACCORD's `agreed` / `ambiguous` / `flagged` states for a two-definition term.
No ACCORD session was run end to end: `build_glossary` was not called, and the claim that these scores drive the glossary rests on reading lines 336 and 356, not on running the phase [s-glossary-not-run].

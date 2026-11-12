# The candidates that are on no page, and why

A candidate reaches a page of dispatch://INFRA-3134/artifact/judgement-md when it can be argued in one page and when something of ours would change.
These were weighed and dropped.
Each line names the evidence that drops it, and each id is a row of `claims.jsonl`.

## From CONCORD

- **The negotiation protocol itself**, its two-anchor calibrated scale and its repair operator.
  Our one instance of its problem is the reviewer's pair, where both halves report a binary verdict pinned to a commit and there is no scale to calibrate [d-28].
  Its own evaluation settles most runs in the first round, so the repair operator — the novel part — runs in a minority of them [d-46, d-47].

- **The reference library**, `packages/ioc-concord`.
  The library and the specification in the same repository are two different protocols: three of the library's five request tags have no counterpart in the specification, and six of its reply tags have none [tc-voc-2].
  The specification requires exact rational arithmetic and the library divides floats under a tolerance [tc-tol-1].
  Driving a real grader disagreement of ours through it worked and decided our own outcome only at one threshold our record does not fix [tc-run-4].

- **The A2A binding.** Envoy is ahead of it on routing, identity, dedupe, receipts and holder lifecycle; the one thing it has that Envoy has not is a digest of the carried content, which is the `quotes` field's `token` on the Envoy page of the judgement rather than a binding to adopt [d-43].

- **Both ACCORD skills and the merge script.** Its merge rules exist only as a script's behaviour, its term test merges a difference of a hundredfold, a thousandfold and an inversion, and a conflict is retained rather than resolved [s-pairs-ours, d-60].

## From SCOPE

- **Neo4j, ClickHouse, RabbitMQ, Redis and the five worker images.**
  Fifteen containers in Compose and eighteen pod-producing objects in the charts, whose declared limits sum to about forty-three CPU and seventy-eight gibibytes [d-93, d-94, d-96].

- **The storage layer.** Its only write primitive overwrites in place with no history, and its embedding vector is stored as a string property with no index [d-81, d-82].

- **The semantic-group machinery.** Our tree is known at admission; it would rediscover a noisy approximation of it by cosine distance over session embeddings [d-99].

- **The session-end trigger.** Its pipeline starts on a `session.end` span, polled every three seconds, and our role sessions are suspended, resumed and retired by rule rather than announcing an end [d-97].

- **The ninety-odd v1 API routes** and its trimmed copy of a price table [d-112].

- **`enabledEvidence`**, which records how we know a protocol was enabled and qualifies a regular expression over a markdown bullet, so a change of renderer changes its report while the world is unchanged [d-108].

What is kept from SCOPE is on the judgement's execution-record page, and it is OpenTelemetry's vocabulary rather than SCOPE's code.

## From MAS Lab

- **Its specification format for Legion's phase workflow.** No format in `ctl` holds a whole row of our table: the workflow block holds an entry and nodes, the standalone workflow document holds from, to and a condition and is executed by nothing, and the state-machine table of `library-standard` has no trigger field and no effect field [m-ctl-answer-1, tm-gap-trigger, tm-gap-effects].
  Their loader does load thirty-eight transitions written as their table, and the guard, which is a Go function value, has no evaluator at all [tm-table-loads].

- **`library-kg`.** Its store is one rebuilt document per run and its graph write overwrites in place with no writer and no cause [m-kg-save-overwrite].

- **`library-telemetry`'s own vocabulary.** It mixes real `gen_ai.` names with two namespaces of its own, and one of those is absent from the OpenTelemetry registry [m-tel-ioa-graph-not-otel].

- **Every other directory of the repository**, which the triage drops by class: a service beside Legion, or a rebuild in Go or TypeScript [t-ml-drop-libraries, t-ml-drop-ui, t-ml-drop-infra].

## From the organisation's other two repositories

- **`ioc-project.github.io`** is the documentation site: prose, quoted where a claim cites it, copied nowhere else [t-site-licence].

- **The `.github` repository** holds the organisation profile page and no licence, so its prose may be quoted and nothing of it may be copied [t-dg-no-licence].

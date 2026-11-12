# The node model, read back

This page reads the node model against the work of INFRA-3134 and answers four questions about it.
It reads `dispatch://INFRA-1098/spec` at version 97 and `dispatch://INFRA-1253/spec` at version 17.
Both were read with `dispatch doc-read` pinned to those version numbers, so the read is re-runnable and its bytes are fixed: the v97 read is `sha256:16df219221e8cec5ab29cf0d83aca31235d4066d8d02fefd09e92592be091fdd` [n-read-1098] and the v17 read is `sha256:3c1eb50debe6f6ac1b8e84f1a97f48ab34c3b69be5a0934198dd3bf68545ab81` [n-read-1253].
Version 97 of INFRA-1098's spec carries an approval line, `approved v97 by sami@theorem.dev` [n-read-1098]; version 17 of INFRA-1253's spec carries no approval line at all [n-read-1253].

A sentence quoted from either specification is evidence of what that specification says will exist, and never of what Dispatch does today.
Each such row is marked as a claim of the project, not of its code.

## 1. Does the model hold a finding with its evidence and its verdict?

No, not as a node of its own, and this section says exactly how far it gets.

The measured starting point is our own schema.
`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json` declares `"version": 3` and five typed blocks, `callout`, `ask`, `requirement`, `plan_task` and `check` [n-blocks-v3].
A search of that file for `finding`, `verdict`, `evidence`, `definition`, `glossary` and `vocabulary` returns nothing [n-blocks-absent].
INFRA-1098's spec states the same count in its own words: "Today `pmdoc/schema/blocks.json` is at version 3. It holds the five types `callout`, `ask`, `requirement`, `plan_task` and `check`." [n-1098-says-blocks].

What the two specifications say will be added to that schema is two block types and no more.
"The `decision` type and the `reading` type of R4 ship in one block schema version bump, from 3 to 4." [n-type-bump-4].
Neither a `finding` type nor a `verdict` type is among them.

A verdict does exist in the model, but it belongs to a check's result and to nothing else.
INFRA-1253 R12 says "A result is a node with its tuples, under the `result` supertag." and names its values: "Those are its verdict, its surface, its run, what the command printed, its commit, its role, its counts of failed, skipped and excluded cases, and its negative control." [n-verdict-result].
So the model's verdict is a scalar of a node that records one run of one check.

The evidence a verdict stands on is likewise the record of a command.
The values just quoted are what a command printed, the commit it ran at, the role that ran it, three case counts and a negative control [n-gap-evidence-kind].
INFRA-1253 R3 puts a rule behind the last of these: "`recordCheckResult` in `api/results.go` refuses `verdict: pass` on a `before_merge` check whose `negative_control` is empty, and the refusal states the field." [n-verdict-negctl].

A second kind of verdict is computed on read rather than written.
INFRA-1253 R8 says "A requirement with no check is a gap. A requirement whose only check holds no result is unproven. A requirement whose only result is a fail is unproven." [n-verdict-states].

A finding is held only as the text of a lens node.
INFRA-1098 R25 says "The lens node hangs under the node it read, through `part_of`, and carries that lens's finding." [n-finding-lens].
The nearest thing to a finding with a typed relation is the risk under a lens: "A risk node points at the check that prevents it through a `prevented_by` tuple, or at nothing, and a risk that points at nothing reads as prevented by nothing rather than as a fault." [n-risk-prevented].
A node also carries a state: INFRA-1253 R7 says "A node overtaken by events carries that as the third value of the node state attribute." [n-node-state], beside `proposed` and `settled`.

Neither specification names a `finding` node kind, a finding type, or an evidence relation.
The search over both pinned reads returns nothing [n-no-finding-kind].
The word `verdict` appears six times in INFRA-1253's spec and not once in INFRA-1098's [n-verdict-scope].

So the model holds a verdict for a check's run, a finding as free text on a lens, and no typed evidence at all.
A claim row of the kind this issue produces, which carries an id, a source, a kind, a mark and an evidence object, has no node in the model today.

## 2. Are the three alignment problems answered by its spec, plan and verification nodes?

### Agreement on the plan before the work

Partly.
The plan is a query over the same graph: INFRA-1253 R8 says "Spec, Verification, Plan and Decisions are not separate trees and not separate documents: each is a filter of that one graph, drawing its own node kinds out of it, so pulling the requirement nodes gives the spec and pulling them with their checks gives the verification criteria." [n-align-plan-graph].
A query node can be approved in principle, because R8 says "Each query node keeps a row in the `artifacts` projection, so it is addressable, versioned and approvable" and R14 says "The request points at a query node of an issue." [n-align-plan-query-approvable].

But no sentence of either specification names a plan query or a plan approval.
The search over both pinned reads for `plan query`, `plan approval`, `approves the plan` and `the plan is approved` returns nothing [n-align-plan-absent].
Both specifications name the two queries an approval reads, and the plan is not one of them: "The verification query takes the issue's `check` nodes and the `result` nodes under them. The spec query takes its criterion and requirement nodes." [n-align-verify-approval].
Our own code agrees: `model.GatedStepsKnown` holds `decisions`, `criteria` and `spec`, and no plan step [n-gates-no-plan].

The agreement the model does gate before the work is on the criteria and the requirements.
INFRA-1098 R28 says "The spec approval takes the same rule over the spec query, which returns the issue's criterion and requirement nodes, and nothing else" [n-align-spec-approval].
And a child issue is left out of that: INFRA-1253 R22 says "A child issue gets no conversation tree at all, and no approval of its own" [n-align-child-no-approval].

### Agreement on the verification criteria after the work

Yes.
INFRA-1098 R28 says "The query of a Verification approval reads the issue's `check` nodes and the `result` nodes under them, with their attributes and their words." [n-align-verify-criteria].
INFRA-1253 R14 names the same two queries [n-align-verify-approval] and states what moves the approval: "Recording a result moves the verification digest." [n-align-verify-digest].
So the criteria a person approved and the results recorded against them are one hashed record, and a later result is visible as a move of that digest.

### Agreement on the terms used

Nothing answers it.
The search over both pinned reads for `glossary`, `vocabulary`, `definition node`, `term node` and the back-quoted words `definition` and `term` returns nothing [n-align-terms-absent].
The model has a pattern that would carry it, the open catalogue of R25 [n-cand-defs-catalogue], but no requirement applies that pattern to the words an issue uses.

## 3. The gaps

**A finding is not a node, so a judgement cannot be stored where it is read** [n-gap-finding].
Evidence: neither pinned read names a finding kind or an evidence relation [n-no-finding-kind]; a verdict exists only as a value of a `result` node bound to one check run [n-verdict-result]; a finding exists only as the free text a lens node carries [n-finding-lens].
What would close it: one node kind whose values are the mark and the claim, and one relation from it to the node it judges, declared the way R25 declares a lens, so that a judgement of a spec hangs under that spec and reads back with its author and its time.
INFRA-1098 R18 prices that work: "A node kind costs four declarations, and they land in one change." [n-cand-defs-cost].

**Evidence has one shape, the record of a command** [n-gap-evidence-kind].
Evidence: the values a result node carries are what a command printed, its commit, its role, three case counts and a negative control [n-verdict-result].
A cited address with its fetch date, a file and symbol at a commit, or an empty search with its scope, each of which this issue's own claim rows carry, has no field and no relation in the model.
What would close it: an evidence node under the finding, with a kind value and the fields that kind needs, so that a verdict states which of its evidence rows it rests on.

**The plan is a query that nothing approves** [n-gap-plan-gate].
Evidence: the plan is a filter of the one graph [n-align-plan-graph]; a query node is approvable [n-align-plan-query-approvable]; and no sentence of either specification names a plan query or a plan approval [n-align-plan-absent], while our own `model.GatedStepsKnown` holds `decisions`, `criteria` and `spec` [n-gates-no-plan].
What would close it: one sentence in INFRA-1253 naming the plan query's node set, the way R14 names the spec query's and the verification query's, and one more value for `gated_steps` in our code.

**No record holds the terms an issue uses** [n-gap-terms].
Evidence: the search over both pinned reads returns nothing for every spelling of a glossary, a vocabulary, a definition node or a term node [n-align-terms-absent].
What would close it: a catalogue node per project whose children are the terms, reached by the `has_kind` pattern R25 already uses for lenses [n-cand-defs-catalogue], so a word is a row rather than a schema change.

## 4. The three candidate changes, once an issue is one graph of typed nodes

Read against INFRA-1098's spec at version 97 and INFRA-1253's spec at version 17.

### (a) One optional field on the Envoy message envelope carrying `quotes: [{ref, token}]`

It still makes sense, and the model gives the reason it must stay a quotation rather than a placement [n-cand-quotes-verdict].
INFRA-1253 R2 separates the two: "They admit `dispatch://<ISSUE>/<ask|comment|message>/<full lower-case uuid>#b-<encoded id>`, unpinned, because a parent is a place and not a quotation." [n-cand-quotes-parent].
A quote therefore needs its own pinning, which is what the token is, and the model answers the other half of staleness itself: "One appended `moved_to` tuple records each move, from the old address to the node." [n-cand-quotes-moved-to].
The reference the field would carry survives the cutover, because a message becomes a node of the graph: "A message to an agent is a node written under that page." [n-cand-quotes-message-node].
Today our envelope carries no such field: `contracts.Envelope` declares nineteen JSON fields and none of them is `quotes` [n-envelope-no-quotes].

### (b) One definitions record from which every term in an issue is drawn

It still makes sense, and the model's own catalogue pattern is the shape it should take [n-cand-defs-verdict].
INFRA-1098 R25 states that pattern: "`lens` is the node kind, and which lens a node is comes from a `has_kind` tuple to a catalogue node of kind `lens_kind`, not from a `kind` value carried on the lens node, so a lens added later is a row rather than a schema change and the catalogue is open rather than closed." [n-cand-defs-catalogue].
Nothing in either specification claims this ground: no definition node and no term node is named in either [n-align-terms-absent].
The price is stated: "A node kind costs four declarations, and they land in one change." [n-cand-defs-cost].

### (c) An execution record of our own under the OpenTelemetry `gen_ai.*` names

It still makes sense, and it does not collide with the model, because the model's own run record is a node inside Dispatch and says nothing about a span or a metric name [n-cand-exec-verdict].
INFRA-1098 R18 is the model's execution record: "An `interaction` node and a `changed_by_run` tuple hold the rows of dispatch://INFRA-1042's `going_requests` and `going_events` tables, which are on main." [n-cand-exec-interaction].
A result already points at the run that produced it: "Its check, its recorder, its run's session and each of its screenshots are tuples out of that node." [n-cand-exec-result-tuples].
Neither specification mentions OpenTelemetry, `otel`, `gen_ai`, a trace or a metric [n-cand-exec-no-otel].
So the two records answer different readers, and a span emitted under the `gen_ai.*` names is the outside name for the same run the `interaction` node holds inside.

## 5. What INFRA-323's defining-terms draft already proposes

`dispatch://INFRA-323/artifact/defining-terms-draft-md` is at version 2 [n-323-version] and carries no approval line [n-323-no-approval].
It is a draft of a skill, so it proposes a procedure for a definitions record rather than a schema for one.

It fixes where the record lives: "Each project has ONE definitions file: a `Vocabulary` section in the repository's instruction file, or a glossary file that instruction file links." [n-323-one-file].

It already answers the case of words no repository owns: "Words a repository does not own (a vocabulary two teams share, the words that reach one person) live on the tracking issue, and the instruction file links them there, so an agent with no checkout can still read them." [n-323-shared-words].

It fixes the shape of an entry: "Term — what it is in plain words — the code's word in brackets — the source of any number." [n-323-entry-shape], with "One sentence per entry." [n-323-entry-one-sentence].

It settles who may coin a word, which version 2 added after a person asked for it: "If the list has no word for a concept, describe the concept in words the list already has." [n-323-no-coined].

Our own definitions page therefore answers work already begun.
It should name this draft, take its entry shape, and say what it adds: which record an agent reads when the words belong to no repository, and how a term reads back once an issue is one graph of typed nodes.

## 6. What INFRA-2918's delivery and pipeline metrics are

The delivery numbers are the four delivery measures against the owner's targets over a window, which `GET /api/v1/delivery/measures` answers as deploys a day, lead time, change failure rate per pull request and per deploy, time to restore, rework share, deploy-run success, a per-UTC-day series and the open P0 issues with no owner [n-2918-routes].
The pipeline numbers are the last seven UTC days of CI by the day each run started, which `GET /api/v1/delivery/pipeline` answers as the pull-request checks workflow's wall time and success, its five slowest jobs, the jobs that failed in the last 24 hours, the duration and result of the staging end-to-end, staging deploy and production deploy jobs, and merge-to-production and pull-request-open-to-merge per merge day against their targets [n-2918-routes].

INFRA-2918 draws both on one page: "The tab shows, in this order: the target cards, the Legion measures, CI health, deploys and lead time, and the merge timeline, under one header holding the window, the compare range, the freshness line and the filters." [n-2918-sections].
Each card carries a verdict of its own: "The target cards are main's six, with the three failure measures, Legion work time and review rounds beside them, each read from its eval row with its verdict and its change against the compare range." [n-2918-cards].

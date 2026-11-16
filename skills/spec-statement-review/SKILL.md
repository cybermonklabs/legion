---
name: spec-statement-review
description: "Use when you review a change request of The Spec, even one that `dispatch spec-form` finds clean. A change request is a proposed spec of statements, checks and terms, before its approval request."
---

# Spec statement review

You review a change request of The Spec: the spec of an issue that proposes statements, checks and terms.
Review only a change request that you did not write, because an author reads its own words with the meaning it intended.

The form check of Dispatch decides the count of sentences and words, the ASD-STE100 rules and the links to records of Dispatch.
It also decides the name of an incident, a pull request or a person.
You judge what no program decides, and nothing else, so that one rule never gets two answers.

## Steps

1. Run `dispatch spec-form --issue <KEY> --artifact <document>`.
   It prints the findings of the form check, one line for each: `- <kind> <node>: <detail>.`
   Dispatch decided each finding already, so do not report it again.
   Then run the same command with `--terms`, which prints the list of terms in place of the findings.
   The list has one line for each term: `<term>: <definition> (<document> version <n> of <KEY>)`.

2. Read the change request with `dispatch doc-read --issue <KEY> --artifact <document>`.
   A statement is a `:::requirement{#<id>}` block or an entry line ``- **<statement>** `<id>` · under: …``, with `-`, `*` or `+`.
   Its node is its id.
   A check is a `:::check{#<id> …}` block: its text is the claim, `run` is the procedure, and `serves` names its statements.
   The terms of the request are the lines under its heading `Terms`, at any level, that start with `-`, `*` or `+`, then `**<term>**: <definition>`.
   Such a line in a statement or a check adds no term.
   A heading with other text, such as `**Terms**`, `Terms:` or `terms`, is a finding of the form check, and it opens no terms.
   Nothing in a code fence or in a quote, a line that starts with `>`, is a statement, a check, a term or a heading.
   Then run `dispatch read --issue <KEY>`, which lists the documents of the issue.
   When they include `verification.md`, run `dispatch spec-form` and `dispatch doc-read` with `--artifact verification.md` too.

   When a command fails, print `NOT REVIEWED: <the command> failed: <the first line of its error>` and stop.
   A review without the list of terms cannot find an undefined noun, and `NO FINDING` would tell the author a false thing.

3. Read each statement for these two kinds.

   - `principle`: a statement must state a specific of the system.
     A specific is what a part of the system does, holds or refuses, in a case that a check can show.
     Where it names a quantity, it gives the limit and the unit, such as "at most 20 findings" or "after 10 minutes".
     Name a statement of a value, an aim or a quality with no act that a check can show, such as "the daemon is reliable".
     Also name a statement with a quantity that has no limit or no unit, such as "Legion answers fast".

   - `agent-choice`: no statement says what an agent may or may not do while it works on the code.
     Name a statement that only the choice of an agent can make true, such as "the planner reads the goals before it writes a plan".
     What the software does for an agent is a specific, such as "the daemon gives each planner the goals in its first prompt".

   A rule on how the code is built is a specific, such as "only one component writes the phase of an issue".
   A description of how the code works is a specific too, such as "the daemon keeps its waiting line in memory".
   Do not name a statement that holds a rule, a description or both.

4. Read each statement for its nouns of the system.
   A noun of the system names a thing that Legion, Dispatch or Envoy makes, keeps, sends or shows.
   To test a noun, put its usual English meaning in its place.
   If the statement then says less, or not what the thing holds, it is a noun of the system.
   Otherwise it is a common noun, such as page, minute, file or database.
   A role or a state in them is a noun of the system too.
   The names Legion, Dispatch and Envoy are not nouns of the system.
   A word in code font is not one either.
   When the list defines such a noun, it is a term.
   A term matches in its singular and its plural.

   - `undefined-noun`: name each noun of the system that neither the list nor the terms of the request define.
     Write one line for each noun, at the first statement that uses it.

   - `other-meaning`: name a term that a statement uses with a meaning other than its definition.
     Write one line for each term, at the first statement that uses it with that other meaning.

   - `repeated-meaning`: name a new term of the request whose definition gives the meaning of a term of the list.
     Its node is `terms`.

   A second definition of a term of the list is a finding of the form check, so do not report it.

5. Read each check against the statements that it serves.

   - `wide-claim`: a claim must be narrower than its statement.
     Name a check whose claim holds a case, an actor, a route or a limit that its statements do not hold.

   - `agent-where-program`: an agent is the procedure only where no program can decide the claim.
     Name a check whose procedure is an agent that reads what a program can decide.
     Examples are a status code, a count, a field, an exact text and an order of events.

   These are right, so do not name them:

   - A statement with several checks, such as one of a normal case, one of a boundary and one of a failure.
     Several claims are no reason to split a statement.

   - A claim that is narrower than its statement.

   - A query check or an agent check that names no test.
     Only a check that a program runs names its test and its job.

   - An agent check whose claim needs a judgement of meaning, such as whether a text states a principle.

6. Print one line for each finding, and no other text:

   ```text
   FINDING <kind> <node> <subject>: <one sentence>
   ```

   - `<kind>` is one of the seven kinds.
     The lines come in this order of kinds: `principle`, `undefined-noun`, `other-meaning`, `repeated-meaning`, `wide-claim`, `agent-choice`, `agent-where-program`.

   - `<node>` is the id of the statement or of the check, with no `#`, or `terms`.

   - `<subject>` is the noun or the term as the text writes it, for the three kinds of step 4.
     It is `-` for the other four kinds.

   - The sentence says what is wrong.

   When you find nothing, print `NO FINDING` and no other text.

   One line of each kind, then the line for no finding and the line for a failed command:

   ```text
   FINDING principle lease-safety -: It states an aim, and no act of the daemon that a check can show.
   FINDING undefined-noun slot-release slot: Neither the list nor the terms of the request define slot.
   FINDING other-meaning phase-timeout phase: The list defines phase as a step of the workflow of an issue, and this statement uses it for a time window.
   FINDING repeated-meaning terms work item: Its definition gives the meaning of issue, which the list defines.
   FINDING wide-claim v-lease-expiry -: The claim holds the lease of each role, and its statement holds only the lease of a planner.
   FINDING agent-choice planner-reads-goals -: Only the choice of the planner can make it true.
   FINDING agent-where-program v-claim-refused -: An agent reads an exit status, which a program can compare with 1.
   NO FINDING
   NOT REVIEWED: dispatch spec-form --issue ACME-4 --artifact spec.md --terms failed: Dispatch did not answer
   ```

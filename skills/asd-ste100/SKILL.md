---
name: asd-ste100
description: "Use when English text must be read with no human to resolve ambiguity (tool descriptions, error messages, agent instructions, system prompts, status reports), reads as dense or easy to misparse, or needs an STE100 or plain-language rewrite. Not for creative or marketing copy."
---

# Simplified Technical English (ASD-STE100)

## Two modes

Pick a mode before you rewrite. If the user does not name one, infer it from the text type.

- **Strict**: procedures, error messages, tool and function descriptions, instructions to an agent, safety text, and anywhere a wrong reading has a cost. Apply every rule below.
- **STE-flavored**: READMEs, PR descriptions, changelogs, explanatory prose. Apply the structural rules in full; treat the lexical rules as advice, since prose needs some range of words.

## Structural rules

| Rule | Do | Don't |
|---|---|---|
| Active voice in procedures and instructions | "The agent deletes the file." | "The file is deleted (by the agent)." Passive only in descriptive text, when the actor is unknown or irrelevant |
| Safety first | Open a safety-critical instruction with its command or condition: "Stop the pump before you open the valve." | The command or condition buried mid-sentence |
| No phrasal verbs (Rule 9.3) | "Remove the panel." / "Start the job." | "Take off the panel." / "Spin up the job." |
| One instruction per sentence | "Open the file. Read line 3." | "Open the file and read line 3, then check if it matches." |
| Sentence length | ≤20 words for instructions and procedures, ≤25 for descriptions | Long compound or subordinate-clause sentences |
| No semicolons (Rule 8.1) | Separate sentences | Any semicolon. Every other mark is allowed; an em dash often marks a sentence to split. |
| Noun clusters | ≤3 nouns stacked ("fuel pump valve") | 4 or more ("high pressure fuel pump inlet valve assembly") |
| No ellipsis | Keep the subject, verb and article | "Files not backed up will be lost" (which files?) |
| Keep modality | "The request **may have** failed." stays as it is | Turning a hedge into a fact, or adding a certainty the source did not state |
| Paragraphs | One topic, ≤6 sentences | Several topics in one paragraph |
| Lists for sequences | A numbered or bulleted list for 3 or more steps or conditions | A sequence buried in one sentence |

## Lexical rules

These come from ASD's dictionary of about 900 approved words, which ASD does not let others reproduce, so this skill does not hold it. Apply them as a direction, and never claim dictionary compliance. Where exact approved wording matters, get the standard from https://www.asd-ste100.org/ and check word by word.

- One word, one meaning: one term for one thing or action through the whole text.
- One part of speech per word: "Apply oil to the valve", not "Oil the valve".
- Verb, not noun (Rule 3.7): "Analyze the log", not "Perform an analysis of the log".
- Keep necessary technical terms, and define each once if it is not common English.

## Simple tenses

Use the infinitive, imperative, simple present, simple past, simple future, and the past participle as an adjective: "we received the report", not "we have received the report". Where a compound form carries information the simple form cannot ("the job has completed" as current state, or the hedge in "may have failed"), keep it. When the tense rule and the modality rule conflict, modality wins.

## Scan checklist

Scan for all six before you rewrite:

1. **Synonym rotation**: one thing with several names ("the user", "the customer", "the client"). Pick one.
2. **Hedge stacking**: qualifiers that leave no claim ("it is important to note that this may potentially help"). State the claim or delete it.
3. **Nominalization**: "perform an analysis of", "provides assistance to". Use the verb.
4. **Marketing adjectives**: seamless, robust, powerful, effortless. Delete, or give the measurement.
5. **Run-on sentences**: ideas joined by semicolons or em dashes. One idea per sentence.
6. **Soft phrasal verbs**: spin up, reach out, dive into, kick off. Use start, contact, read, begin.

## Process

1. Pick the mode.
2. Read the text once for meaning before you change anything.
3. Walk it sentence by sentence, and flag each break of the rules and each checklist habit. In STE-flavored mode, flag the lexical rules but do not enforce them.
4. Rewrite each flagged sentence with the same meaning. Keep every hedge ("may", "could", "is likely to") at its strength, and add no cause, frequency or mechanism the source did not state. Where a rewrite would drop a safety condition, a scope qualifier or a number, keep the longer form.
5. Stop when each sentence has one reading, not when it is shortest.
6. If the text already complies, say so and change nothing.

Read `skill://asd-ste100/examples/before-after.md` when you want worked rewrites of agent output, each with what was flagged and what was kept.

## Output

By default, print the rewritten text and nothing else: no preamble, mode, count, summary or offer. If step 4 kept a longer form on purpose, add one line after the text, `Kept as-is:`, naming the phrase and the precision it keeps.

When the user asks for the reasoning ("show the diff", "which rules did it break", "before/after"), print this table instead, then one line on what you did not simplify and why:

```markdown
| Rule violated | Original | Simplified |
|---|---|---|
| Present perfect tense | "We have received your request." | "We received your request." |
| Noun cluster (4+ words) | "the agent task queue priority handler" | "the handler that sets task-queue priority" |

Mode: Strict. 7 violations found.
```

STE fixes the form of a text, not its substance. If the text has nothing to say, say so rather than polish it.

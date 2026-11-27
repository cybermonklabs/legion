# Rubric: the judged items of owner-walk

Judge each item that a run's `judge-input.md` lists, as `pass` or `fail`, from that file alone.

**Done when:** `judgment.json` beside `judge-input.md` holds one entry for each item that the file lists, and no other entry.

## Step 1: Read the input

1. **Read `judge-input.md` whole.** It holds each text that an item needs: the spec of the planned issue, the record of the earlier issue, the owner's plan, and, where an item needs them, the output of the read of earlier plans, the guidance, the questions and the owner's answer.
2. **Judge from that file alone.** Do not judge from the transcript, from another file of the run, or from another run. The name of the run's directory says which label and form the run had: let it decide no verdict.

## Step 2: Judge each item

Each item is one sentence of the check `v-guidance-judge`. Judge it by its row of the table. When the text an item needs is missing (no plan, no read, no answer), the item fails.

| Item | The check's sentence | It passes when | It fails when |
| --- | --- | --- | --- |
| `choices` | says, for each step, whether a fresh agent or the agent of an earlier step works it, and why | For each step of the plan, the plan says which agent works it: a fresh agent, or the agent of an earlier step (of this issue or of an earlier issue). Each choice has its reason. One sentence can cover more steps when it names them or says "each step". | One step has no choice, or one choice has no reason. A plan that says only "use the default agents", or names agents with no reason, fails. |
| `earlier-and-fresh` | names the earlier agent for the first task, and a fresh agent for the second | For the first task (finish the CSV importer), the plan names the agent that did the earlier work: the session `rig-implementer-e1`, or the implementer of the earlier issue by its key. For the second task (the guide to the error messages), it names a fresh agent: one that saw none of the importer's work. Both tasks can stand in one task block. | The first task gets no earlier agent. Or the second task gets the earlier agent, or an agent of the first task. |
| `cites` | cites what dispatch plans printed | The plan names at least one fact that the read's output holds: a key of a finished issue it lists, the steps of such a plan, a rating, or the words of a rating's reason. It says what the plan follows or changes because of that fact. | The plan speaks of earlier plans in general and names no fact of the output. Or the input holds no output of the read. |
| `not-yet-quotes` | answers that the review is not yet on another family, and it quotes the guidance | The answer says no: the review is not on another model family than the implementer, or not yet. It also quotes the guidance: words of the guidance in quotation marks or a block quote, or five words or more of it in a row. | The answer says yes, or it gives no answer to the question. Or it quotes no words of the guidance. |
| `yes` | answers yes and names both models | The answer says yes: the model of the review step is of another family than the implementer's. It names the model of the implementer and the model of the reviewer, as the guidance names them (a prefix such as `anthropic/` or `openai/` before an id is the same model). | The answer says no, names one model only, or names the families with no model. |

## Step 3: Write the judgment

1. **Write one entry for each listed item.** `verdict` is `pass` or `fail`. `why` is one sentence that quotes the words of the run that decided the verdict, or says which text is missing.
2. **Write `judgment.json`** beside `judge-input.md`:

   ```json
   {"run": "<the run's directory name>", "items": [{"item": "choices", "verdict": "pass", "why": "<one sentence>"}], "by": "<your session id>", "at": "<the output of date -u +%FT%TZ>"}
   ```

3. **Run `score.ts runs <runs dir> --run <the run's directory name>` again.** It reads the judgment and writes the run's row.

{{define "grade-phrases"}}You check sentences of a text. {{template "dispatch" .}} Find each phrase that states no fact: decide, for each candidate sentence, whether its verb phrase is vacuous. A vacuous verb phrase is a finite verb that sounds like an action but states no fact. The reader cannot tell from it what happened, what will happen or what the agent asks.

## The user message

- "candidates": the candidate sentences, each with its id, the two sentences before it ("before") and the sentence after it ("after"). Use "before" and "after" only to find facts. Judge only the candidate sentence.

{{template "data" .}}

## The rule

1. Find each finite verb of the sentence of these forms: name, show, pay, carry, hold, land, arrive, unblock, surface, in any tense, active or passive, and the phrases the reader asked agents to stop (below). A finite verb has its own subject and tense: "the run carries", "it landed", "the files are named".
2. Skip a verb in a quotation, in code, in an identifier, in the title of a pull request or in a person's own words. Skip a noun, an adjective, or a participle with no finite verb of its own ("the named file", "a step named in the notes", "returns while showing no rows", "carried over from the last draft"). Skip a phrasal verb with its own literal sense ("carry out", "hold off").
3. Take the verb phrase out and ask: "What literally happens?" If the sentence or the sentences next to it give the fact the verb stands for, the verb phrase is not vacuous. The fact is a value: a literal string, an identifier, a number, a count, a list of items. Or a holder (a person, a role, a team), a date or a time, an amount of money, or an observed event (a test fails, a command exits 1, a page displays a line). A word such as "them" can point to the value in the sentences before.
4. The rules of the verbs:
   - name: literal when a person or an agent says or writes the name ("the files you named", "name the job next to it"), or the verb means "give a name to". Not vacuous when the sentence or the sentences next to it give the value, the holder, the date or a count ("the check names two files, a.go and b.go"). Vacuous in the sense "contains the name of" with no value ("the log names the failing step", "the two modes are named"); the reader refused that sense.
   - show: literal when a display surface (a page, a view, a card, a chart or a part of it, a dashboard, a preview, a log, a diff, a command's output) displays the thing. A term of art when it means "prove" in mathematics with the proposition stated. Not vacuous when the sentence gives a value or an observed event of a named test or run ("the rerun showed that writes stop when the disk fills"). Vacuous when the object is an abstract noun (the gap, the cause, nothing, what X does), a verdict with no observation ("shows that the fix works", "shows no strain"), or when "show" has no object. A report or a document that "shows" a thing is vacuous unless the sentence gives the value.
   - pay: vacuous for "pays off", "pays for itself", "pays its way", and for a grader that "pays" an answer or a proof, even with a score. Literal when money moves: a payer pays an amount, a person or a bill. Not vacuous when the sentence gives an amount of money. Vacuous otherwise: what is "paid" is a cost, a time, a build, a scan, a row or nothing.
   - carry, hold: not vacuous when the verb means "contains" or "has" and the sentence gives the value or names the thing ("the commit carries the line Fixes: ABC-1", "the list holds three users: ana, bo and cy", "the copy carries the same bug", "the archive keeps all it holds", "a job may carry 12 files"), or says that a named thing holds nothing of a kind ("both jobs carry no result yet"). Literal when a principal holds a grant, a role, a lock or a lease, or a date or a plan "holds" (stays). Vacuous when the object has no value ("carries the finding", "holds rows that a purge removed"), in the sense "take to" or "bring to" ("carries the result to the board", "carry it to the team"), and with no object ("if the job carries").
   - land, arrive: not vacuous when the sentence says when or where the thing lands, or what landing is there: a time ("it landed at 09:40Z"), a place ("lands with its test in one pull request", "lands on main"), a signal that reaches a process, a pull request or change that merges, a delivery, a person or a message that arrives. Vacuous when the sentence says only that a thing lands, with no time, no place and no sense of merge, deploy, ship or run ("the fix lands soon", "until both land", "the step lands").
   - unblock, surface and the other verbs: vacuous when you cannot say from the sentence what happens ("this unblocks the work", "the draft surfaces the risk").
   - If one of two objects of the verb has no value, the verb phrase is vacuous.
5. The verdict: "vacuous" when one verb phrase of the sentence is vacuous. "unresolved" when no verb phrase is vacuous but one cannot be settled: its object is a pronoun whose value is not in the sentences you have, or the word can be a verb or a noun and the sentence has no other finite verb. Else "not_vacuous".

The reader asked agents to stop these phrases. Each, in its vague sense, is a vacuous verb phrase:

{{.Phrases}}

## Your answer

Give one JSON object with the field "candidates": one item for each candidate, in the order given. Each item has these fields, in this order:

- "1_id": the candidate's id.
- "2_reason": one sentence that says what literally happens, or which fact is missing.
- "3_rewrite": when the verdict is "vacuous", the sentence again without the vacuous verb, from facts in the input only, with a missing fact in square brackets ("The log gives the failing step [the step is not in the text]."); else "".
- "4_verdict": "vacuous", "not_vacuous" or "unresolved".{{end}}

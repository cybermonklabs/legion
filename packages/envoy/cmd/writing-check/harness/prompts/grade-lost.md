{{define "grade-lost"}}You compare two texts. {{template "dispatch" .}} The original text is the text that the agent wrote. The new text is the text that the reader sees after a change to the original text.

Find each fact of the original text that the new text lost.

## The user message

- "original_text": the original text.
- "new_text": the new text.
{{- template "question-tag" .}}
{{- template "context-tags" .}}

{{template "data" .}}

## How to find a lost fact

{{template "fact" .}}

- A fact is lost when the original text states it and the new text does not state it.
{{- if .LostKinds}}
- A state or a time that the new text changes is a lost fact (for example, a thing that the original text calls done, merged or decided, and that the new text calls planned, under way, open or proposed), and so is a name, an account, a path, a number or a date that the new text no longer gives.
{{- end}}
- A fact that the new text states in other words is not lost.
- A fact that the new text states in a link, a reference or a list is not lost.
- Do not list a phrase of the original text that states no fact.
- List each lost fact one time.

These words of the original text state no fact:

- A heading, such as a bold line of a few words over a part of the text.
- A label alone on its line, such as "The choice:". A label with a fact after it, such as "State: done.", states that fact.
- A word of agreement or thanks, such as "OK.", "Agreed." or "Thanks."
- A line that says again what another sentence of the original text says.
- The reader's own words, quoted back to him.

## Your answer

Give one JSON object with the field "lost": the list of the lost facts. The list is empty if the new text lost no fact. Each item has these fields, in this order:

- "1_quote": the words of the original text that state the fact. Copy them exactly.
- "2_states_no_fact": true if the words are a heading, a label, a word of agreement, a line that says again what another sentence says, or the reader's own words quoted back, else false.
- "3_in_new_text": true if the new text states the fact, in other words, in a link, a reference or a list, else false.
- "4_fact": the fact, in one sentence.

Do not list a fact for which "2_states_no_fact" or "3_in_new_text" is true.{{end}}

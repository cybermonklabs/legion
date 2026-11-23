{{define "grade-added-verify"}}You check facts against the inputs of a text. {{template "dispatch" .}} The original text is the text that the agent wrote. The new text is the text that the reader sees after a change to the original text. The change had these inputs: the original text, the record{{if .HasSession}}, the session of the writer agent{{end}}{{if .WriterAnswer}} and the answer of the writer agent{{end}}.

A first check listed facts of the new text that it found in no input. For each listed fact, decide whether an input holds it.

## The user message

- "original_text": the original text.
{{- template "question-tag" .}}
{{- template "context-tags" .}}
- "listed_facts": the facts that the first check listed. Each fact has a number, the fact in one sentence, and the words of the new text that state it.

{{template "data" .}}

## How to decide

{{template "fact" .}}

- An input holds a fact when it states the fact or shows it directly.
- An input that states the fact in other words holds it.
- An input holds a fact that follows directly from it, such as a sum of its numbers.
- No input holds a guess, a cause or a result that no input states.
- The new text is not an input. Its words do not hold a fact.
{{- if .HasQuestion}}
- The question is not an input. Its words do not hold a fact.
{{- end}}

## Your answer

Give one JSON object with the field "facts": one item for each listed fact, in the order of the list. Each item has these fields, in this order:

- "1_number": the number of the fact in the list.
- "2_input_words": the words of one input that state the fact or show it. Copy them exactly from that input. Give "" if no input holds the fact.
- "3_held": true if an input holds the fact, else false.

"3_held" is true only when "2_input_words" gives the words that hold the fact.{{end}}

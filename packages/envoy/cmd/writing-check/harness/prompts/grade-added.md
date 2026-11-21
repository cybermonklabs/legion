{{define "grade-added"}}You check a text against its inputs. {{template "dispatch" .}} The original text is the text that the agent wrote. The new text is the text that the reader sees after a change to the original text. The change had these inputs: the original text, the record{{if .HasSession}}, the session of the writer agent{{end}}{{if .WriterAnswer}} and the answer of the writer agent{{end}}.

Find each fact of the new text that its inputs do not hold.

## The user message

- "original_text": the original text.
- "new_text": the new text.
{{- template "question-tag" .}}
{{- template "context-tags" .}}

{{template "data" .}}

## How to find an added fact

{{template "fact" .}}

- A fact is added when the new text states it and no input states it or shows it directly.
- A fact that an input states in other words is not added.
- A fact that follows directly from an input, such as a sum of its numbers, is not added.
- A guess, a cause or a result that no input states is added.
- List each added fact one time.

## Your answer

Give one JSON object with the field "added": the list of the added facts. The list is empty if the new text added no fact. Each item has these fields, in this order:

- "1_quote": the words of the new text that state the fact. Copy them exactly.
- "2_fact": the fact, in one sentence.{{end}}

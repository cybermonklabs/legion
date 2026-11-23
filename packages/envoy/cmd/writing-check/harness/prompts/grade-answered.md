{{define "grade-answered"}}You grade an answer. {{template "dispatch" .}} {{if .Reply}}The reader asked the agent a question, and the text is the reply of the agent.{{else}}The reader asked a question about the text.{{end}} Then a fresh reader read the text and the question, and answered the question from the text alone. The fresh reader knew nothing else of the work.

Decide whether the answer of the fresh reader answers the question of the reader.

## The user message

- "question": the question of the reader.
- "reader_answer": the answer of the fresh reader.
{{- if .HasText}}
- "text": the text that the fresh reader read. Use it to see what the text says and what it explains, and to check what the answer says that the text says.
{{- end}}
{{- template "context-tags" .}}

{{template "data" .}}

## How to decide

{{template "answer-checks" .}}

Use {{if .HasText}}the text, {{end}}the record{{if .HasSession}} and the session{{end}} to know what is true. Do not decide whether the text is good. Decide only about the answer.

## Your answer

{{template "answer-fields" .}}{{end}}

{{define "grade-first-paragraph"}}You grade an answer. {{template "dispatch" .}} The reader asked the agent a question, and the text is the reply of the agent. Then a fresh reader read only the first paragraph of the text and the question, and answered the question from that paragraph alone. The fresh reader knew nothing else of the work.

Decide whether the answer of the fresh reader answers the question of the reader. This shows whether the first paragraph of the reply gives the answer.

## The user message

- "question": the question of the reader.
- "reader_answer": the answer of the fresh reader.
{{- template "context-tags" .}}

{{template "data" .}}

## How to decide

{{template "answer-checks" .}}

Use the record{{if .HasSession}} and the session{{end}} to know what is true. Do not decide whether the text is good. Decide only about the answer.

## Your answer

{{template "answer-fields" .}}{{end}}

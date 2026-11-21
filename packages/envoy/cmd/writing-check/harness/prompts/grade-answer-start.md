{{define "grade-answer-start"}}You find an answer in a text. {{template "dispatch" .}} The reader asked the agent a question, and the text is the reply of the agent.

Find the first sentence of the text that answers the question.

## The user message

- "question": the question of the reader.
- "text": the reply of the agent.
{{- template "context-tags" .}}

{{template "data" .}}

## How to find it

- A sentence answers the question when it gives what the reader asked for: the fact, the reason, the state or the decision. When the question asks whether something is true, a sentence that says yes or no to it answers it, even a short one, such as "Yes, and the fix uses it."
- A sentence that only says what the reply is about, what the agent did, or what is done, does not answer, unless that is what the reader asked.
- Find the first such sentence. A later sentence that gives more detail does not count.

## Your answer

Give one JSON object with these fields, in this order:

- "1_quote": the first words of the first sentence that answers the question: at least eight words, or the whole sentence if it is shorter. Copy them exactly, with the same words in the same order. If no sentence answers the question, give an empty string.
- "2_found": true if a sentence of the text answers the question, else false.{{end}}

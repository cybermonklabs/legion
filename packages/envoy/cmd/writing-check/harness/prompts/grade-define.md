{{define "grade-define"}}You grade a definition. {{template "dispatch" .}} The reader selected words in the text and asked what they mean. A definition of those words was written for him.

Decide whether the definition answers the question of the reader.

## The user message

- "question": the question of the reader, with the words he selected.
- "definition": the definition that the reader sees.
{{- template "context-tags" .}}

{{template "data" .}}

## How to decide

First, find the words that the reader asks about.

Then do these four checks on the definition:

- "3_restates_words": true when the definition only says the words again, or says them in other words, and gives no new fact. For example, the reader asks what "the run is green" means, and the definition is "It means that the run is green".
- "4_points_elsewhere": true when the definition says only where the meaning is, that the record does not tell, or that the reader must ask someone.
- "5_other_words": true when the definition says what other words mean, and not the words that the reader asks about.
- "6_contradicts_record": true when the record{{if .HasSession}} or the session{{end}} shows that the definition is wrong.

If one check is true, the definition does not answer the question.

If all four checks are false, the definition answers the question only when it says what the words mean where the text uses them, in words that the reader can understand without a new question. A guess does not answer.

## Your answer

Give one JSON object with these fields, in this order:

- "1_asked": one sentence that says which words the reader asks about.
- "2_given": one sentence that says what the definition says they mean.
- "3_restates_words", "4_points_elsewhere", "5_other_words", "6_contradicts_record": true or false, as the checks say.
- "7_reason": one to three sentences that say why the definition answers the question or not.
- "8_answers": true if the definition answers the question, else false. When one check is true, "8_answers" is false.{{end}}

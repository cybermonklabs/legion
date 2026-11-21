{{define "grade-harder"}}You compare two texts. {{template "dispatch" .}} The reader read the original text with no question about its words. The new text is the text that the reader sees after a change to the original text.

Decide whether the new text is harder to read for the reader than the original text.

## The user message

- "original_text": the original text.
- "new_text": the new text.
{{- template "question-tag" .}}
{{- template "context-tags" .}}

{{template "data" .}}

## How to decide

The new text is harder to read when one or more of these is true, and nothing in the new text makes up for it:

- The reader must read more words to get the same facts.
- It uses a word or a name that the reader must ask about, and the original text does not.
- Its sentences are longer or harder to follow.
- The fact or the decision that the reader needs first comes later.
- It uses more phrases that state no fact.

If the two texts are the same, the new text is not harder to read. Do not decide whether a fact was lost.

## Your answer

Give one JSON object with these fields, in this order:

- "1_reason": one to three sentences that say why.
- "2_harder": true if the new text is harder to read than the original text, else false.{{end}}

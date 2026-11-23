{{define "grade-undefined"}}You check a text. {{template "dispatch" .}} The reader knows the work in general, but he did not see what the agent did.

Find each word or name of the text that the reader would have to ask about: he cannot understand the sentence that uses it from the text and the record.

## The user message

- "text": the text, as the reader sees it.
{{- if .HasOwnWords}}
- "reader_own_words": the words of the text that the reader wrote himself, in a text of his own, before the agent wrote the text. Each word is in lower case.
{{- end}}
{{- template "question-tag" .}}
{{- template "context-tags" .}}

{{template "data" .}}

## How to find an undefined word or name

A word or a name is undefined only when all of these are true:

- The text uses it.
- The text does not say what it means or what it is.
- The record does not say what it means or what it is.
- It is not a word of common English, not a common word of software work (such as session, pull request, review, test, branch, commit, deploy, key, server or login), and not a well-known tool, product, standard or term of the field (such as a compiler, a cloud service, a machine type or a canary deploy), used in its usual meaning.
- The reader needs to know what it is to understand what the sentence says.

A word or a name is defined when one of these is true:

- The text says what it means or what it is, in the same sentence or in another sentence, also in a few words, such as "the two image kinds (A and B)".
- The record says what it means or what it is: in the title of the issue, in a document, or in a text of the thread.
- A person wrote it in the record, in a text of the thread that a person wrote: the reader knows the words he uses himself.
{{- if .HasOwnWords}}
- It is in "reader_own_words": the reader wrote it himself before, so it is not undefined for him.
{{- end}}

These are the usual kinds of undefined words. Look for them first:

- A common word that the writer uses with a sense of his own, such as a build that is "shelved", the "mouth" of a queue, work "in the air", a "bench" of jobs or a "ticket" of a rollout. The word is common, but its sense in the text is not, so it is a finding. Ask: would the reader know what the writer means by this word here?
- A term that the writer made up, such as a "staircase" of a rollout or a "bucket" of a method.
- A short form or an abbreviation that the text does not spell out.
- A code name of a task family, a group, a process, a step or a kind of record that the sentence needs, when neither the text nor the record says what it is.

These are not findings:

- A name given as a reference: a file, a function, a lemma, a setting, a check, a job, a machine, a version, a label or a tool in a list, when the sentence says what it does with it and the reader can follow the sentence without knowing more about it. For example, in "The nightly script stopped at its third step because the disk was full", the script is a reference.
- The plain name of a system, a service, a product, a repository, a project, an agent or an organization of the reader's work, used as a name: the reader leads this work and knows its systems. A term inside one of them, such as a kind of record, a role or a unit of work that the system has, can still be a finding.
- A word that is vague, or that does not say which thing it is, and a phrase that is not a name ("for a while", "the two open items", "set up"). A vague word is not an undefined word.
- A word in inline code, between backticks: a value, a field, a table, a state or an id that the text quotes.
- A link, an issue key (such as ABC-123), an id of a requirement, a check, a step, a slice, a section or an option of a document (such as R1, A15 or "Option B"), the label of an option that the text offers, the name of a person or of an account, a number, a date or a time.
- A word of the work that the record uses itself as a known term: the record's issue title, documents or thread use it the same way, as a word their reader knows.
- A word in a code block, a command or a path that the text gives only for the reader to copy.
- A word that is not in the text.

List each word or name one time. If no word or name is undefined, give an empty list. An empty list is a correct answer.

## Your answer

Give one JSON object with the field "undefined": the list of the undefined words and names. Each item has these fields, in this order:

- "1_word": the word or the name. Copy it exactly as the text has it.
- "2_defined_in_text": true if the text says what it means or what it is, else false.
- "3_defined_in_record": true if the record says what it means or what it is, a person wrote it in the record,{{if .HasOwnWords}} it is in "reader_own_words",{{end}} or the record uses it itself as a known term of the work, else false.
- "4_common_meaning": true if it is a word of common English, a common word of software work, or a well-known tool, product, standard or term of the field, used in its usual meaning, else false. A common word in a sense of the writer's own is false.
- "5_reference_only": true if it is a name given as a reference, an id or an option label, a word in inline code, or the plain name of a system, a service, a product, a repository, a project, an agent or an organization of the work, and the reader can follow the sentence without knowing more about it, else false.
- "6_why": one sentence that says what the reader cannot know about it.

Do not list a word for which "2_defined_in_text", "3_defined_in_record", "4_common_meaning" or "5_reference_only" is true.{{end}}

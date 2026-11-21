{{define "define-question"}}A person reads your {{.Kind}} {{.ID}}. They selected these words in it and asked what they mean:
{{range .Words}}
- {{.}}
{{- end}}

For each of these words, say in plain words what you mean by them in this {{.Kind}}. Use one to three sentences for each. Do not use a word that the reader cannot know.
{{end}}

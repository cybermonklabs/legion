{{define "rewrite-question"}}A person asked Dispatch to write your {{.Kind}} {{.ID}} again, so that they can {{if .Ask}}answer it{{else}}understand it{{end}} without your conversation. Dispatch writes the new text from its record and from your answer to this question.

What must the reader know to {{if .Ask}}answer the ask{{else}}understand the text{{end}}? Give these facts in plain words:
{{- if .Ask}}

{{template "ask-parts" true}}
{{- end}}

If you use a word or a name that the reader cannot know, say what it means. Give only facts that you know. Do not change the {{.Kind}} yourself.

The {{.Kind}}:

{{.Text}}
{{end}}

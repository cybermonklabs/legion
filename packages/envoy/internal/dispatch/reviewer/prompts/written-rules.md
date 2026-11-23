{{define "written-reader-words"}}## Words that this reader asked about

{{.Reader}} asked what these words mean, or said that a text with these words was not clear. You may use these words. Where you use one of them, say beside it, in a few plain words, what it means.

{{range .Words}}- {{.}}
{{end}}{{end}}

{{define "written-ask-parts"}}## The parts of an ask

An ask must give the reader these five parts:

{{template "ask-parts" true}}
{{end}}

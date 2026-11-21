{{define "written-reader-words"}}## Words that this reader asked about

{{.Reader}} asked what these words mean, or said that a text with these words was not clear. Do not use these words. If you must use one of them, write in plain words what it is.

{{range .Words}}- {{.}}
{{end}}{{end}}

{{define "written-ask-parts"}}## The parts of an ask

An ask must give the reader these five parts:

{{template "ask-parts" true}}
{{end}}

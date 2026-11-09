package api

import "net/http"

// scratchReflectedAnswer is a scratch measurement of this repository's code-scanning rule, on a
// pull request closed without merging. This is the control: the same handler with the query
// parameter taken out, so CodeQL reports nothing. Nothing calls it.
func scratchReflectedAnswer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<p>a scratch answer</p>"))
}

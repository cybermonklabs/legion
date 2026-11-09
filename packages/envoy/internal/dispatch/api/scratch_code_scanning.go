package api

import "net/http"

// scratchReflectedAnswer is a scratch measurement of this repository's code-scanning rule, on a
// pull request closed without merging. It writes a query parameter into an HTML answer, which
// CodeQL reports as go/reflected-xss at high severity. Nothing calls it.
func scratchReflectedAnswer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<p>" + r.URL.Query().Get("q") + "</p>"))
}

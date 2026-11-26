package api

import (
	"strings"
)

// oneLine is a statement's SQL with its whitespace run together, as a failure prints it.
func oneLine(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

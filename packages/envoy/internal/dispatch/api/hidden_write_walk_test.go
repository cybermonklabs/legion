package api

import (
	"strings"
)

// volatileStatement reports whether a statement is the stamp of volatileColumns' column, which a
// token's first use in a minute writes and its next does not, whatever either call names.
func volatileStatement(statement string) bool {
	return strings.HasPrefix(statement, "update agent_tokens set last_used_at = now()")
}

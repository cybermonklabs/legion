// The Stage 4a agent-secrets checks' pure logic: the shell-quoting `agent-secrets <args>` runs
// through kubectl exec with. It touches no cluster, so — unlike live_test.go and
// live_secrets_test.go, both `//go:build e2e` — this file carries no build tag: ordinary
// `go test ./...` compiles and exercises it directly (live_secrets_unit_test.go).
package sandbox

import "strings"

// shellJoin quotes args for a POSIX shell -c string, the way (*liveRig).agentSecrets builds the
// command kubectl exec runs in a pod's container.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

package leakguardtest

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// RecordsGitCall is the start of a stand-in git: it appends to the file at record one line per
// call, with its arguments, the variables the guard's read with no credential sets, must not
// inherit or must keep, what its HOME holds, and the directory it runs in, each as key=value and
// separated by tabs (ReadCalls). The guard's own tests and `legion gh`'s, which reaches that read
// through the guard, read the same record.
func RecordsGitCall(record string) string {
	return `printf 'argv=%s\tprompt=%s\taskpass=%s\tsshaskpass=%s\trequire=%s\tnosystem=%s\tglobal=%s\tcount=%s\tkey0=%s\tparameters=%s\tgitdir=%s\tprotocol=%s\tcainfo=%s\tcapath=%s\tproxy=%s\tlang=%s\thome=%s\tnetrc=%s\thomeholds=%s\tdir=%s\n' ` +
		`"$*" "${GIT_TERMINAL_PROMPT-unset}" "${GIT_ASKPASS-unset}" "${SSH_ASKPASS-unset}" "${SSH_ASKPASS_REQUIRE-unset}" ` +
		`"${GIT_CONFIG_NOSYSTEM-unset}" "${GIT_CONFIG_GLOBAL-unset}" "${GIT_CONFIG_COUNT-unset}" "${GIT_CONFIG_KEY_0-unset}" ` +
		`"${GIT_CONFIG_PARAMETERS-unset}" "${GIT_DIR-unset}" "${GIT_ALLOW_PROTOCOL-unset}" ` +
		`"${GIT_SSL_CAINFO-unset}" "${GIT_SSL_CAPATH-unset}" "${HTTPS_PROXY-unset}" ` +
		`"${LC_ALL-unset}" "${HOME-unset}" "${NETRC-unset}" "$(ls -A "${HOME-/nonexistent}" 2>&1 | tr '\n' ' ')" ` +
		`"$(pwd -P)" >> '` + strings.ReplaceAll(record, "'", `'\''`) + "'\n"
}

// ReadCalls is each call a stand-in recorded in record, a line of key=value fields separated by
// tabs each, as RecordsGitCall writes them, as its keys and values, and none where the stand-in
// never ran.
func ReadCalls(t testing.TB, record string) []map[string]string {
	t.Helper()
	content, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []map[string]string
	for line := range strings.Lines(string(content)) {
		call := map[string]string{}
		for field := range strings.SplitSeq(strings.TrimRight(line, "\n"), "\t") {
			key, value, _ := strings.Cut(field, "=")
			call[key] = value
		}
		calls = append(calls, call)
	}
	return calls
}

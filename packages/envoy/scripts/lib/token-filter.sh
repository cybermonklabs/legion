# shellcheck shell=bash
# packages/envoy/scripts/lib/token-filter.sh: the record rule's filter for the lines a script prints,
# sourced by scripts/daemon-release-gate.sh and loadtest/cluster/cluster.sh; it is not run on its own.
#
# Whatever answers gh's calls in GitHub's place holds gh's token. A script keeps gh's own words and
# any answer it does not take out of what it prints, but a reading that holds to its grammar (an
# image reference, a repository's URL, a file path), or a tool's error that quotes one, can still
# carry a whole run of the token. So each such line passes redact_lines, which writes every run of 8
# characters of the token as [redacted]: the run the Go code's redaction rule refuses
# (redacttest.Piece). A run shorter than 8 characters, and a credential the file does not hold
# (docker's own login for a registry, npm's), are not filtered.

# token_filter_init FILE WHO: writes FILE with the credentials the filter reads, one a line: the
# token gh uses, as `gh auth token` reads it (its config, or the variable that overrides it), then
# GH_TOKEN, GITHUB_TOKEN, GH_ENTERPRISE_TOKEN and GITHUB_ENTERPRISE_TOKEN where set. FILE is made
# under umask 077, so only this user reads it, and no token is ever in a process's arguments. When
# none of them is 8 characters or more, it says so on stderr, naming WHO: such a run filters nothing.
token_filter_init() {
  (
    umask 077
    {
      gh auth token 2>/dev/null || true
      printf '\n%s\n%s\n%s\n%s\n' "${GH_TOKEN:-}" "${GITHUB_TOKEN:-}" "${GH_ENTERPRISE_TOKEN:-}" "${GITHUB_ENTERPRISE_TOKEN:-}"
    } >"$1"
  )
  if ! grep -qE '^.{8,}$' "$1"; then
    echo "$2: gh auth token answered no token, and no token variable is set, so no line below is filtered for one" >&2
  fi
}

# redact_lines FILE: stdin to stdout, line by line as each arrives, with every run of 8 characters of
# a credential in FILE (token_filter_init's) written as [redacted], a stretch of such runs that touch
# or overlap as one. A token shorter than 8 characters has no such run and is not filtered.
redact_lines() {
  python3 -c '
import sys

with open(sys.argv[1], encoding="utf-8", errors="surrogateescape") as handle:
    tokens = [t for t in handle.read().split("\n") if len(t) >= 8]
pieces = {t[i : i + 8] for t in tokens for i in range(len(t) - 7)}
out = sys.stdout.buffer
for raw in iter(sys.stdin.buffer.readline, b""):
    line = raw.decode("utf-8", "surrogateescape")
    hidden = [False] * len(line)
    for piece in pieces:
        at = line.find(piece)
        while at >= 0:
            hidden[at : at + 8] = [True] * 8
            at = line.find(piece, at + 1)
    shown, i = [], 0
    while i < len(line):
        if hidden[i]:
            while i < len(line) and hidden[i]:
                i += 1
            shown.append("[redacted]")
        else:
            shown.append(line[i])
            i += 1
    out.write("".join(shown).encode("utf-8", "surrogateescape"))
    out.flush()
' "$1"
}

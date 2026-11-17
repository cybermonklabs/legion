#!/usr/bin/env bash
# Tests for `neutralize-private-keys.py`: each form of a project key it replaces, each text it
# leaves, its renames, its refusals, and that a second run changes nothing.
#
# Every case builds a git repository under a temporary directory with a copy of the script and runs
# it there, with a term list of placeholder projects (QUILL, ZORB) that name no real one. The
# placeholder each key must get is computed here from the rule the script's header states, with
# sha256sum, so a change to the rule fails these cases rather than moving with them.
#
# Run from anywhere: .github/scripts/neutralize-private-keys.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$script_dir/neutralize-private-keys.py"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# placeholder <KEY>: the header's rule. As many capitals as the key has characters, the n-th
# being "A" plus the n-th byte of SHA-256("neutral-project-key:v1:" + KEY) modulo 26.
placeholder() {
  local key=$1 hex out='' byte i
  hex=$(printf 'neutral-project-key:v1:%s' "$key" | sha256sum)
  for ((i = 0; i < ${#key}; i++)); do
    byte=$((16#${hex:2*i:2}))
    # shellcheck disable=SC2059 # the format is the octal escape of one capital
    out+=$(printf "\\$(printf '%03o' $((65 + byte % 26)))")
  done
  printf '%s' "$out"
}
PQ=$(placeholder QUILL)
PZ=$(placeholder ZORB)
TERMS=$'Some Private Term\n  key:QUILL \nkey:ZORB\nkey:LEGION\n'
# The link scheme in pieces, so that no line of this file holds a whole one, as the script under
# test holds none.
scheme='dispatch'':''//'
scheme_upper='DISPATCH'':''//'

# fixture <name>: an empty git repository holding the script at its path. Echoes its path.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts"
  cp "$script" "$root/.github/scripts/neutralize-private-keys.py"
  git -C "$root" init -q
  git -C "$root" config user.email test@example.com
  git -C "$root" config user.name test
  echo "$root"
}

commit() { git -C "$1" add -A && git -C "$1" commit -q -m fixture; }

# run <root> [args...]: the script in root with TERMS, its output and its exit as "$out" / "$status".
run() {
  local root=$1
  shift
  status=0
  out=$(cd "$root" && PRIVATE_NAME_TERMS=$TERMS ./.github/scripts/neutralize-private-keys.py "$@" 2>&1) || status=$?
}

# template <file> <template>: writes the template to file with @K@ as QUILL, @k@ as quill, @S@
# as the link scheme and @SU@ as the scheme in capitals, and returns, on stdout, the text the
# script must leave: the same with @K@ as QUILL's placeholder and @k@ as its lower case.
template() {
  local text=${2//@S@/$scheme}
  text=${text//@SU@/$scheme_upper}
  mkdir -p "$(dirname "$1")"
  local written=${text//@K@/QUILL}
  printf '%s' "${written//@k@/quill}" > "$1"
  local left=${text//@K@/$PQ}
  printf '%s' "${left//@k@/${PQ,,}}"
}

echo "the placeholder rule"
check "the two placeholder keys get placeholders of their own lengths" \
  "$( [[ $PQ =~ ^[A-Z]{5}$ && $PZ =~ ^[A-Z]{4}$ && $PQ != QUILL ]] && echo true || echo false)"

echo "each form a key takes is replaced, the number kept"
root=$(fixture forms)
forms=$'bare @K@-12 and (@K@-7).\n'
forms+=$'in backticks `@K@-12`, in a link [@K@-12](https://tracker.example/issues/@K@-12).\n'
forms+=$'@S@@K@-12 @S@@K@-12/spec#b-x @S@@K@-12/artifact/notes-md@v3\n'
forms+=$'@S@@K@-12/ask/0b5e7a1c-0000-4000-8000-000000000001 @S@@K@-12/comment/c1\n'
forms+=$'@SU@@K@-9 and a project document @S@@K@/artifact/plan-md\n'
forms+=$'quoted "@K@" \'@K@\' `@K@`\n'
forms+=$'a subject notifications.dispatch.issue.@K@.@K@-12.> and notifications.agent.@K@.s1\n'
forms+=$'roles @K@.legion-x-controller @K@.<role> legion-@K@-architect @K@-controller, the @K@ controllers\n'
forms+=$'built fmt.Sprintf("@K@-%d", n) `@K@-${n}` "@K@-" + n, escaped @K@\\-12 and @K@\\\\-12, after an escape "\\n@K@-100"\n'
forms+=$'search @K@ 1042 and @K@:1\n'
forms+=$'after project:@K@ legion-daemon:@K@ /projects/@K@/x \\/projects\\/@K@ number_project=@K@ Project @K@\n'
forms+=$'tokens legion-@k@-controller legion-@k@-1000-architect legion-@k@-@k@-12-reviewer notifications.role.legion-@k@-controller "legion-@k@-\\n"\n'
stays=$'@S@LEGION-1 and the bare @S@ scheme, LEGION-5\n'
stays+=$'a slug quill-12, QUILL-12x, XQUILL-12, QUILL_12, QUILLS, the QUILL team, $QUILL, QUILL.md\n'
stays+=$'no role: legion-quill-amd64.tar.gz, legion-quill-6687cbae.svc, legion-quill-pull-550.json\n'
stays+=$'a value of its own, as YAML writes one: key: QUILL\n'
expect=$(template "$root/docs/forms.md" "$forms$stays")
expect_go=$(template "$root/cmd/x/main.go" $'package main\n\n// Fixed in @K@-3 (@S@@K@-3).\nconst issue = "@K@-4"\n')
expect_ts=$(template "$root/web/x.ts" $'// @K@-5\nexport const key = `@K@-6`;\n')
expect_py=$(template "$root/tools/x.py" $'# @K@-7\nKEY = "@K@-8"\n')
expect_sh=$(template "$root/tools/x.sh" $'# @K@-9\necho "@K@-10"\n')
commit "$root"
run "$root"
check "the run exits 0" "$(is "$status" 0)"
check "the markdown file holds every replaced form and every text left alone" \
  "$(is "$(cat "$root/docs/forms.md")" "$expect")"
check "a Go comment and string" "$(is "$(cat "$root/cmd/x/main.go")" "$expect_go")"
check "a TypeScript comment and template" "$(is "$(cat "$root/web/x.ts")" "$expect_ts")"
check "a Python comment and string" "$(is "$(cat "$root/tools/x.py")" "$expect_py")"
check "a shell comment and string" "$(is "$(cat "$root/tools/x.sh")" "$expect_sh")"
check "the summary counts the lines and files and names no key" \
  "$( [[ $out == *"2 project keys read; rewrote 20 lines in 5 files, 0 paths"* && $out != *QUILL* ]] && echo true || echo false)"

echo "two keys get two placeholders, and one key the same one in every file"
root=$(fixture distinct)
printf 'QUILL-7 and ZORB-7\n' > "$root/a.md"
printf 'QUILL-7\n' > "$root/b.md"
commit "$root"
run "$root"
check "QUILL-7 and ZORB-7 stay two issues" "$(is "$(cat "$root/a.md")" "$PQ-7 and $PZ-7")"
check "QUILL-7 is the same placeholder in another file" "$(is "$(cat "$root/b.md")" "$PQ-7")"

echo "a path that names a key is renamed, and the text naming it follows"
root=$(fixture paths)
mkdir -p "$root/docs/QUILL-4-notes"
printf 'see docs/a-QUILL-12.md\n' > "$root/index.md"
printf 'body\n' > "$root/docs/a-QUILL-12.md"
printf 'nested\n' > "$root/docs/QUILL-4-notes/x.md"
commit "$root"
mkdir -p "$root/notes"
printf 'untracked\n' > "$root/notes/b-ZORB-3.txt"
run "$root"
check "the run exits 0" "$(is "$status" 0)"
check "the tracked file is at its new path, staged as a rename" \
  "$( [ -f "$root/docs/a-$PQ-12.md" ] && [ ! -e "$root/docs/a-QUILL-12.md" ] &&
      git -C "$root" status --porcelain | grep -q "^R  docs/a-QUILL-12.md -> docs/a-$PQ-12.md$" && echo true || echo false)"
check "a directory that names a key moves with the file under it, and the old one is gone" \
  "$( [ -f "$root/docs/$PQ-4-notes/x.md" ] && [ ! -e "$root/docs/QUILL-4-notes" ] && echo true || echo false)"
check "an untracked file is renamed too" \
  "$( [ -f "$root/notes/b-$PZ-3.txt" ] && [ ! -e "$root/notes/b-ZORB-3.txt" ] && echo true || echo false)"
check "the text that names the path names the new one" "$(is "$(cat "$root/index.md")" "see docs/a-$PQ-12.md")"
check "the summary counts three paths" "$(contains "$out" ", 3 paths$")"

echo "a second run changes nothing"
before=$(cd "$root" && git status --porcelain && find . -path ./.git -prune -o -type f -print0 | sort -z | xargs -0 sha256sum)
run "$root"
after=$(cd "$root" && git status --porcelain && find . -path ./.git -prune -o -type f -print0 | sort -z | xargs -0 sha256sum)
check "the second run exits 0 and rewrote nothing" "$( [ "$status" = 0 ] && [[ $out == *"rewrote 0 lines in 0 files, 0 paths"* ]] && echo true || echo false)"
check "the tree after the second run is the tree after the first" "$(is "$after" "$before")"
run "$root" --check
check "--check after it exits 0" "$(is "$status" 0)"
check "--check says no text or path names a key" "$(contains "$out" "no text or path names a project key$")"

echo "--check writes nothing, names each line and path, and names no key"
root=$(fixture check)
printf 'one\nQUILL-1 here\nthree ZORB-2\n' > "$root/a.md"
printf 'x\n' > "$root/QUILL-5.md"
commit "$root"
run "$root" --check
check "--check exits 1" "$(is "$status" 1)"
check "it names each line by the path it will have" \
  "$( [[ $out == *$'a.md:2\na.md:3'* && $out == *"$PQ-5.md: its path"* ]] && echo true || echo false)"
check "it names no key" "$( [[ $out != *QUILL* && $out != *ZORB* ]] && echo true || echo false)"
check "it wrote nothing" "$( [ -z "$(git -C "$root" status --porcelain)" ] && echo true || echo false)"

echo "what the script leaves as it is"
root=$(fixture leaves)
printf 'QUILL-1\0binary\n' > "$root/blob.bin"
printf 'QUILL-1\n' > "$root/bun.lock"
printf 'QUILL-1\r\nline two QUILL-2\r\n' > "$root/crlf.md"
printf 'target QUILL-1\n' > "$root/target.md"
ln -s target.md "$root/link.md"
mkdir -p "$root/scripts/approval-eval" "$root/scripts/approval-evaluation"
printf 'QUILL-1 "QUILL"\n' > "$root/scripts/approval-eval/reserve.ts"
printf 'QUILL-1\n' > "$root/scripts/approval-evaluation/notes.md"
printf 'Some Private Term\n' > "$root/term.md"
printf 'ignored QUILL-1\n' > "$root/ignored.md"
printf 'ignored.md\n' > "$root/.gitignore"
commit "$root"
run "$root"
check "a file holding a NUL byte is left" "$(is "$(od -An -c "$root/blob.bin" | tr -d ' \n')" 'QUILL-1\0binary\n')"
check "a lockfile is left" "$(is "$(cat "$root/bun.lock")" QUILL-1)"
check "a CRLF file keeps its line ends" "$(is "$(od -An -c "$root/crlf.md" | tr -d ' \n')" "$PQ-1\\r\\nlinetwo$PQ-2\\r\\n")"
check "a symbolic link is still a link to its target" "$( [ -L "$root/link.md" ] && [ "$(readlink "$root/link.md")" = target.md ] && echo true || echo false)"
check "the approval experiment is left" "$(is "$(cat "$root/scripts/approval-eval/reserve.ts")" 'QUILL-1 "QUILL"')"
check "a path that only starts like the kept one is not kept" "$(is "$(cat "$root/scripts/approval-evaluation/notes.md")" "$PQ-1")"
check "a term of the list is not this script's to replace" "$(is "$(cat "$root/term.md")" 'Some Private Term')"
check "an ignored file is not read" "$(is "$(cat "$root/ignored.md")" 'ignored QUILL-1')"

echo "--words and --lower-words, on the files they are given and nowhere else"
root=$(fixture words)
printf 'the QUILL team and ZORB\x27s queue, pod quill-12-postgres, token "quill"\n' > "$root/prose.md"
printf 'agent("quill", "QUILL-448"), pod quill-12\n' > "$root/token.go"
printf 'the QUILL team\n' > "$root/other.md"
commit "$root"
run "$root" --words
check "--words with no path is refused" "$( [ "$status" = 2 ] && [[ $out == *"read only the files they are given"* ]] && echo true || echo false)"
run "$root" --words prose.md
check "--words replaces a word that is a key and a lower-case slug, and leaves a lower-case word" \
  "$(is "$(cat "$root/prose.md")" "the $PQ team and $PZ's queue, pod ${PQ,,}-12-postgres, token \"quill\"")"
run "$root" --lower-words token.go
check "--lower-words also replaces a lower-case word that is a key" \
  "$(is "$(cat "$root/token.go")" "agent(\"${PQ,,}\", \"$PQ-448\"), pod ${PQ,,}-12")"
check "and both leave the file not given" "$(is "$(cat "$root/other.md")" 'the QUILL team')"

echo "the refusals"
root=$(fixture refusals)
printf 'QUILL-1\n' > "$root/a.md"
commit "$root"
status=0
out=$(cd "$root" && env -u PRIVATE_NAME_TERMS ./.github/scripts/neutralize-private-keys.py 2>&1) || status=$?
check "no term list is exit 2, naming the missing key lines" "$( [ "$status" = 2 ] && [[ $out == *"holds no key: line"* ]] && echo true || echo false)"
status=0
out=$(cd "$root" && PRIVATE_NAME_TERMS=$'Some Private Term\nkey:LEGION\n' ./.github/scripts/neutralize-private-keys.py 2>&1) || status=$?
check "a list whose only key is LEGION is exit 2" "$(is "$status" 2)"
check "and changed nothing" "$(is "$(cat "$root/a.md")" QUILL-1)"
status=0
out=$(cd "$root" && PRIVATE_NAME_TERMS=$'key:QUILL\nkey:'"$PQ" ./.github/scripts/neutralize-private-keys.py 2>&1) || status=$?
check "a placeholder that is a key of the list is exit 2" "$( [ "$status" = 2 ] && [[ $out == *"share a placeholder, or a placeholder is a key"* ]] && echo true || echo false)"
status=0
out=$(cd "$root" && PRIVATE_NAME_TERMS=$'key:QUILL\n'"${PQ,,}" ./.github/scripts/neutralize-private-keys.py 2>&1) || status=$?
check "a placeholder that holds a term of the list is exit 2" "$( [ "$status" = 2 ] && [[ $out == *"holds a term of the list"* ]] && echo true || echo false)"
check "neither refusal names the term or the key" "$( [[ $out != *QUILL* && $out != *"${PQ,,}"* ]] && echo true || echo false)"
run "$root" --nope
check "an unknown option is exit 2" "$(is "$status" 2)"
printf 'x\n' > "$root/a-$PQ-1.md"
printf 'y\n' > "$root/a-QUILL-1.md"
commit "$root"
run "$root"
check "a rename onto a path that exists is exit 2" "$( [ "$status" = 2 ] && [[ $out == *"exists, so a path that names a key cannot be renamed"* ]] && echo true || echo false)"
check "and the refusal came before any write" "$(is "$(cat "$root/a.md")" QUILL-1)"

echo "the order of the list changes nothing"
root=$(fixture order)
printf 'QUILL-1 ZORB-2 "QUILL"\n' > "$root/a.md"
commit "$root"
first=$(cd "$root" && PRIVATE_NAME_TERMS=$'key:ZORB\nkey:QUILL' ./.github/scripts/neutralize-private-keys.py --check && true; cat a.md)
run "$root"
check "a list in the other order gives the same text" "$(is "$(cat "$root/a.md")" "$PQ-1 $PZ-2 \"$PQ\"")"
check "and the other order's --check named the same line" "$(contains "$first" '^a.md:1$')"

echo "this script names no key of any list"
check "its source holds no whole link scheme, which a pull request's new lines may not hold" \
  "$( ! grep -qiF "$scheme" "$script" && echo true || echo false)"

summary "neutralize-private-keys.py"

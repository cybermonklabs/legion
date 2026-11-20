# shellcheck shell=bash
# shellcheck disable=SC2154 # check-go-offline.test.sh initializes the shared harness variables.

# Cases of check-go-offline.sh's rule 1 in shell test files: a bare site, the offline and the
# reasoned form, what starts nothing, and one planted case for each shell form the check's header
# names. Sourced by check-go-offline.test.sh, whose fixtures, run_check and counters they use; not
# run on its own.

echo "case: a shell test that runs go list -deps with no rule is refused"
root=$(fixture sh-bare)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
go list -deps ./... >/dev/null
EOF
run_check "$root"
refused tools/deps.test.sh 3

echo "case: the shell test with GOPROXY=off on the command passes, and so does an export of it above"
root=$(fixture sh-offline)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
GOPROXY=off go list -deps ./... >/dev/null
EOF
put "$root" tools/exported.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
export GOPROXY=off
go list -deps ./... >/dev/null
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts both commands as offline" "$(contains "$out" 'runs with GOPROXY=off (2), carries a reason (0)')"

echo "case: the shell test with a reason on the line above passes"
root=$(fixture sh-reasoned)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# go-offline: it lists the standard library, which reads no module.
go list -deps std >/dev/null
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the command as reasoned" "$(contains "$out" 'runs with GOPROXY=off (0), carries a reason (1)')"

echo "case: a shell go -C with a quoted directory before its subcommand is a site"
root=$(fixture sh-chdir)
put "$root" tools/deps.test.sh <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
go -C "$root/packages/app" list -deps ./... >/dev/null
EOF
run_check "$root"
refused tools/deps.test.sh 3

echo "case: a go command in a comment, a quoted string or a heredoc's body starts nothing"
root=$(fixture sh-prose)
put "$root" tools/prose.test.sh <<'EOF'
#!/usr/bin/env bash
# go list -deps ./... in a comment starts nothing.
echo "go list -deps ./..."
cat <<'TEXT'
go list -deps ./...
TEXT
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts no command at all" "$(contains "$out" 'every one of the 0 go commands')"


echo "case: shell: bash -c, eval, and a heredoc a shell reads"
refuses sh-bash-c tools/bash.test.sh 2 <<'EOF'
#!/usr/bin/env bash
bash -c 'go list -deps ./...'
EOF
refuses sh-eval tools/eval.test.sh 2 <<'EOF'
#!/usr/bin/env bash
eval "go list -deps ./..."
EOF
refuses sh-heredoc tools/heredoc.test.sh 3 <<'EOF'
#!/usr/bin/env bash
bash <<'SCRIPT'
go list -deps ./...
SCRIPT
EOF

echo "case: shell: the later command of a pipeline needs its own GOPROXY=off"
refuses sh-pipeline tools/pipeline.test.sh 3 2 <<'EOF'
#!/usr/bin/env bash
GOPROXY=off go list -deps ./... |
  xargs go build
EOF
refuses sh-pipeline-line tools/pipeline-line.test.sh 2 <<'EOF'
#!/usr/bin/env bash
GOPROXY=off go list -deps ./... | xargs go build
EOF

echo "case: shell: an export GOPROXY=off inside a function covers that function alone"
refuses sh-function-export tools/function.test.sh 6 <<'EOF'
#!/usr/bin/env bash
setup() {
  export GOPROXY=off
}
setup
go list -deps ./...
EOF
passes_offline sh-same-function tools/function.test.sh <<'EOF'
#!/usr/bin/env bash
build() {
  export GOPROXY=off
  go build ./...
}
EOF

echo "case: shell: the go binary as a path ending /go, and as a variable set to it"
refuses sh-goroot tools/goroot.test.sh 2 <<'EOF'
#!/usr/bin/env bash
"$GOROOT/bin/go" list -deps ./...
EOF
refuses sh-variable tools/variable.test.sh 3 <<'EOF'
#!/usr/bin/env bash
GO=$(command -v go)
"$GO" build ./...
EOF

echo "case: shell: env -u GOPROXY and a later GOPROXY undo an earlier GOPROXY=off"
refuses sh-env-unset tools/unset.test.sh 3 <<'EOF'
#!/usr/bin/env bash
export GOPROXY=off
env -u GOPROXY go list -deps ./...
EOF
refuses sh-later tools/later.test.sh 2 <<'EOF'
#!/usr/bin/env bash
GOPROXY=off GOPROXY=direct go list -deps ./...
EOF

echo "case: shell: each subcommand that can load a module, or fetch a toolchain, is a site"
# A here-string, not a pipe: refuses must run in this shell, where the counters are.
for sub in build run list install get mod test vet env generate work version; do
  refuses "sh-sub-$sub" "tools/sub-$sub.test.sh" 2 <<<"$(printf '#!/usr/bin/env bash\ngo %s' "$sub")"
done

echo "case: shell: go after a function of the script's own, and in a \$(...) and a \`...\`"
refuses sh-after-function tools/retry.test.sh 3 <<'EOF'
#!/usr/bin/env bash
retry() { "$@" || "$@"; }
retry go build ./...
EOF
refuses sh-substitution tools/substitution.test.sh 2 <<'EOF'
#!/usr/bin/env bash
listed=$(go list -deps ./...)
EOF
refuses sh-backtick tools/backtick.test.sh 2 <<'EOF'
#!/usr/bin/env bash
listed=`go list -deps ./...`
EOF

echo "case: shell: env's own GOPROXY=off, an export in the subshell that holds the command, and a -c run with GOPROXY=off are offline"
passes_offline sh-env-offline tools/env.test.sh <<'EOF'
#!/usr/bin/env bash
env GOPROXY=off go list -deps ./...
EOF
passes_offline sh-subshell-export tools/subshell.test.sh <<'EOF'
#!/usr/bin/env bash
( export GOPROXY=off; go list -deps ./... )
EOF
passes_offline sh-c-inherits tools/inherits.test.sh <<'EOF'
#!/usr/bin/env bash
GOPROXY=off bash -c 'go list -deps ./...'
EOF

echo "case: no file under testdata, node_modules, dist, .git or .jj is read"
root=$(fixture skipped-dirs)
for dir in testdata node_modules dist .git .jj; do
  printf '#!/usr/bin/env bash\ngo list -deps ./...\n' | put "$root" "tools/$dir/deps.test.sh"
done
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads none of their go commands" "$(contains "$out" 'every one of the 0 go commands')"

echo "case: shell: a later unset GOPROXY or export -n GOPROXY undoes an export of it"
refuses sh-unset tools/unset-later.test.sh 4 <<'EOF'
#!/usr/bin/env bash
export GOPROXY=off
unset GOPROXY
go list -deps ./...
EOF
refuses sh-export-n tools/export-n.test.sh 4 <<'EOF'
#!/usr/bin/env bash
export GOPROXY=off
export -n GOPROXY
go list -deps ./...
EOF

echo "case: shell: an export in a pipeline or in the background reaches no later command"
refuses sh-export-piped tools/export-piped.test.sh 3 <<'EOF'
#!/usr/bin/env bash
export GOPROXY=off | true
go list -deps ./...
EOF
refuses sh-export-background tools/export-background.test.sh 3 <<'EOF'
#!/usr/bin/env bash
export GOPROXY=off &
go list -deps ./...
EOF

echo "case: shell: a go binary bound by local, \${NAME:-go}, \$(command -v go) or an array"
refuses sh-local tools/local.test.sh 4 <<'EOF'
#!/usr/bin/env bash
build() {
  local GO=go
  "$GO" build ./...
}
EOF
refuses sh-default tools/default.test.sh 2 <<'EOF'
#!/usr/bin/env bash
"${GO:-go}" list -deps ./...
EOF
refuses sh-lookup tools/lookup.test.sh 2 <<'EOF'
#!/usr/bin/env bash
"$(command -v go)" list -deps ./...
EOF
refuses sh-array tools/array.test.sh 3 <<'EOF'
#!/usr/bin/env bash
cmd=(go list -deps ./...)
"${cmd[@]}"
EOF

echo "case: shell: a shell's -c inside another shell's -c, and a shell fed by cat, echo or printf"
refuses sh-c-in-c tools/c-in-c.test.sh 2 <<'EOF'
#!/usr/bin/env bash
bash -c "sh -c 'go list -deps ./...'"
EOF
refuses sh-cat-pipe tools/cat-pipe.test.sh 3 <<'EOF'
#!/usr/bin/env bash
cat <<'SCRIPT' | bash
go list -deps ./...
SCRIPT
EOF
refuses sh-echo-pipe tools/echo-pipe.test.sh 2 <<'EOF'
#!/usr/bin/env bash
echo "go list -deps ./..." | bash
EOF
refuses sh-printf-pipe tools/printf-pipe.test.sh 2 <<'EOF'
#!/usr/bin/env bash
printf 'go list -deps ./...\n' | sh
EOF

echo "case: shell: a file named with no shell extension is read when its first line runs a shell"
refuses sh-shebang tools/deps.test 2 <<'EOF'
#!/usr/bin/env bash
go list -deps ./...
EOF
root=$(fixture sh-no-shebang)
printf 'go list -deps ./...\n' | put "$root" tools/notes
run_check "$root"
check "a file with no shebang and no source extension is not read" "$(contains "$out" 'every one of the 0 go commands')"

echo "case: shell: \$\" inside double quotes opens nothing, so a quote after it ends the string"
root=$(fixture sh-dollar-quote)
put "$root" tools/quote.test.sh <<'EOF'
#!/usr/bin/env bash
check "ends at a dollar" "$(contains "$err" "a line that ends in a dollar$")"
# a comment that holds an apostrophe, a stand-in's
check "matches the stand-in's call" "$(contains "$calls" ' go build ./...$')"
EOF
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads the quoted pattern as no go command" "$(contains "$out" 'every one of the 0 go commands')"

# Alone in its file: the check reads a shell file a statement at a time only when its text holds
# a go followed by a subcommand (MAYBE), so this file is read only while that pattern lets a
# continued line stand between the two.
echo "case: shell: a go whose subcommand is on the next line, after a backslash, is a site, alone in its file"
refuses sh-continued tools/continued.test.sh 2 <<'EOF'
#!/usr/bin/env bash
go \
  list -deps ./...
EOF

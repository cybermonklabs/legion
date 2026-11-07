#!/usr/bin/env bash
#
# check-citations.sh resolves every citation the answer of INFRA-1546 carries.
#
#   bash docs/research/temporal-evaluation-2026-11/check-citations.sh
#   bash docs/research/temporal-evaluation-2026-11/check-citations.sh --self-test
#
# It reads the answer live from Dispatch, so it checks what a reader reads rather than
# a copy. It prints one line per citation and ends with
#
#   citations: N resolved, M unreachable, 0 failed
#
# It runs from the repository root, or from anywhere else: it finds its own directory
# and the repository root from there, and it writes nothing.
#
# The grammar it implements, which the answer follows and the directory's README
# states:
#
#   Code         (`packages/daemon/internal/record/store.go`, `record.Postgres.Enqueue`)
#                A repository-rooted path in a code span, a comma and a space, then a
#                dotted identifier in a code span. It resolves when that file holds the
#                identifier's last dotted element as a Go declaration: a function, a
#                method, a type, a var, a const, a member of a grouped var, const or
#                type, or a struct field. No relative path and no "same file".
#   Literal      (`packages/daemon/internal/api/handoff.go`, `"HANDOFF_STALE_ASSIGNMENT"`)
#                The same shape with the second span double-quoted. It resolves on the
#                exact quoted text being in that file. This is the form for a refusal
#                code or any other token that is no declaration.
#   Stored state the `lease_token` column of `outbox`, or the `outbox` table.
#                A table resolves on a create table of that name in a *.up.sql file
#                under packages/daemon/internal/store/migrations; a column resolves
#                when a migration creates that table with the column or alters the
#                table to add it.
#   Experiment   experiment 1 .. experiment 18. It resolves when that experiment's
#                recorded output is committed beside this script, and it counts as a
#                citation, so a section whose only evidence is a measurement is not a
#                section with no citation.
#   Dispatch     A bare dispatch:// reference, resolved by `dispatch read --ref`.
#   Web          An https:// address immediately followed by ` (read YYYY-MM-DD)`. A
#                404 or a 410 fails. Any other answer, and a timeout, print unreachable
#                and do not fail the run, because a merge must not wait on a third
#                party's uptime.
#
# It exits non-zero on: a file that does not hold the identifier it is cited with; an
# identifier cited in the code form that the file holds only as a string; a table no
# migration creates; a dispatch:// reference that does not read; a web address with no
# read date beside it; an address answering 404 or 410; and a document with no citation
# at all, because an empty set of citations is no evidence.
#
# --self-test runs the resolver over a fixture of deliberately broken citations held in
# this script and over a set of well-formed ones, so a tester can show the check
# refuses what it claims to refuse without editing the live document.

set -o errexit -o nounset -o pipefail

readonly answer_ref='dispatch://INFRA-1546/artifact/temporal-evaluation-md'
readonly migrations_dir='packages/daemon/internal/store/migrations'
readonly repo_marker='packages/daemon/internal/workflow/table.go'
readonly web_timeout=20

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly script_dir
repo_root=$(cd -- "$script_dir/../../.." && pwd -P)
readonly repo_root

die() {
	printf 'check-citations: %s\n' "$*" >&2
	exit 1
}

[[ -f "$repo_root/$repo_marker" ]] ||
	die "$repo_root holds no $repo_marker, so it is not the Legion repository's root; run this script from its place in the repository"

# --- the shapes a citation can take -------------------------------------------------

# The backticks below are the markdown code spans a citation is written in, not command
# substitution.
# shellcheck disable=SC2016
readonly \
	code_pattern='\(`[^`]+`, `[A-Za-z0-9_.]+`\)' \
	literal_pattern='\(`[^`]+`, `"[^"]*"`\)' \
	column_pattern='the `[A-Za-z0-9_]+` column of `[A-Za-z0-9_]+`' \
	table_pattern='the `[A-Za-z0-9_]+` table' \
	experiment_pattern='[Ee]xperiment (1[0-8]|[1-9])([^0-9]|$)' \
	dispatch_pattern='dispatch://[A-Za-z0-9._/@#-]+' \
	web_pattern='https://[^][:space:]`")<>]+( \(read [0-9]{4}-[0-9]{2}-[0-9]{2}\))?'

# --- counting -----------------------------------------------------------------------

count_resolved=0
count_unreachable=0
count_failed=0

report() {
	local status=$1 kind=$2 text=$3 detail=${4:-}
	case "$status" in
	resolved) count_resolved=$((count_resolved + 1)) ;;
	unreachable) count_unreachable=$((count_unreachable + 1)) ;;
	failed) count_failed=$((count_failed + 1)) ;;
	esac
	if [[ -n "$detail" ]]; then
		printf '%-11s %-7s %s: %s\n' "$status" "$kind" "$text" "$detail"
	else
		printf '%-11s %-7s %s\n' "$status" "$kind" "$text"
	fi
}

# A reference at the end of a sentence carries the sentence's punctuation, which is no
# part of the reference.
without_trailing_punctuation() {
	local text=$1
	while [[ -n "$text" ]]; do
		case "$text" in
		*. | *, | *';' | *: ) text=${text%?} ;;
		*) break ;;
		esac
	done
	printf '%s\n' "$text"
}

# --- resolving one citation of each kind --------------------------------------------

# A Go declaration of NAME in FILE: a function, a method, a type, a var, a const, a
# member of a grouped var, const or type, or a struct field. A grouped member and a
# field are both an indented line whose first word is the name, which is why a name
# that appears only inside a string or only in a call does not match.
go_declares() {
	local file=$1 name=$2
	grep -qE \
		-e "^func[[:space:]]+${name}[[:space:]]*[([]" \
		-e "^func[[:space:]]*\([^)]*\)[[:space:]]*${name}[[:space:]]*[([]" \
		-e "^(type|var|const)[[:space:]]+${name}([[:space:]]|\[|$)" \
		-e "^[[:space:]]+${name}([[:space:]]+[^=]|[[:space:]]*=|,|[[:space:]]*$)" \
		-- "$file"
}

resolve_code() {
	local text=$1 path=$2 dotted=$3 name file
	name=${dotted##*.}
	if [[ ! "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]]; then
		report failed code "$text" "$dotted is no Go identifier"
		return
	fi
	if [[ "$path" = /* || "$path" == *..* ]]; then
		report failed code "$text" "$path is no repository-rooted path"
		return
	fi
	file="$repo_root/$path"
	if [[ ! -f "$file" ]]; then
		report failed code "$text" "the repository holds no $path"
		return
	fi
	if go_declares "$file" "$name"; then
		report resolved code "$text"
	elif grep -qF -- "\"$name\"" "$file"; then
		report failed code "$text" "$path holds $name only as a string; cite it as (\`$path\`, \`\"$name\"\`)"
	else
		report failed code "$text" "$path holds no Go declaration of $name"
	fi
}

resolve_literal() {
	local text=$1 path=$2 literal=$3 file
	file="$repo_root/$path"
	if [[ ! -f "$file" ]]; then
		report failed literal "$text" "the repository holds no $path"
		return
	fi
	if grep -qF -- "$literal" "$file"; then
		report resolved literal "$text"
	else
		report failed literal "$text" "$path does not hold $literal"
	fi
}

# The migration that creates TABLE, printed empty when none does.
creating_migration() {
	local table=$1
	grep -lEi "^[[:space:]]*create table[[:space:]]+(if not exists[[:space:]]+)?${table}[[:space:]]*\(" \
		-- "$repo_root/$migrations_dir"/*.up.sql 2>/dev/null | head -n 1 || true
}

resolve_table() {
	local text=$1 table=$2 migration
	migration=$(creating_migration "$table")
	if [[ -n "$migration" ]]; then
		report resolved table "$text" "created by ${migration##*/}"
	else
		report failed table "$text" "no migration under $migrations_dir creates $table"
	fi
}

resolve_column() {
	local text=$1 column=$2 table=$3 migration

	# Added to the table later.
	migration=$(grep -lEi "^[[:space:]]*alter table[[:space:]]+${table}[[:space:]]+add column[[:space:]]+${column}([[:space:]]|$)" \
		-- "$repo_root/$migrations_dir"/*.up.sql 2>/dev/null | head -n 1 || true)
	if [[ -n "$migration" ]]; then
		report resolved column "$text" "added by ${migration##*/}"
		return
	fi

	# Declared in the table's own create table.
	migration=$(creating_migration "$table")
	if [[ -z "$migration" ]]; then
		report failed column "$text" "no migration under $migrations_dir creates $table"
		return
	fi
	if awk -v table="$table" -v column="$column" '
		{ line = tolower($0) }
		!inside && line ~ ("^[ \t]*create table[ \t]+(if not exists[ \t]+)?" tolower(table) "[ \t]*\\(") { inside = 1; next }
		inside && line ~ "^[ \t]*\\)" { inside = 0 }
		inside && line ~ ("^[ \t]*" tolower(column) "[ \t]") { found = 1 }
		END { exit found ? 0 : 1 }
	' "$migration"; then
		report resolved column "$text" "declared by ${migration##*/}"
	else
		report failed column "$text" "${migration##*/} creates $table without a $column column, and no migration adds one"
	fi
}

resolve_experiment() {
	local text=$1 digit=$2 recorded=()
	mapfile -t recorded < <(find "$script_dir" -maxdepth 1 -type f -name "experiment-${digit}-*.txt" -size +0 | sort)
	if ((${#recorded[@]} > 0)); then
		report resolved exp "$text" "recorded in ${recorded[0]##*/}"
	else
		report failed exp "$text" "no recorded output experiment-${digit}-*.txt beside this script"
	fi
}

resolve_dispatch() {
	local ref=$1
	if dispatch read --ref "$ref" >/dev/null 2>&1; then
		report resolved dispatch "$ref"
	else
		report failed dispatch "$ref" 'dispatch read --ref does not read it'
	fi
}

resolve_web() {
	local text=$1 url=$2 dated=$3 code status=0
	if [[ "$dated" != yes ]]; then
		# shellcheck disable=SC2016  # the backticks are markdown, not substitution
		report failed web "$text" 'no read date beside it; write it as `<address> (read YYYY-MM-DD)`'
		return
	fi
	code=$(curl --silent --location --max-time "$web_timeout" \
		--output /dev/null --write-out '%{http_code}' -- "$url" 2>/dev/null) || status=$?
	if ((status != 0)); then
		report unreachable web "$text" "the address did not answer within ${web_timeout}s (curl exit $status)"
	elif [[ "$code" == 404 || "$code" == 410 ]]; then
		report failed web "$text" "the address answers $code"
	elif [[ "$code" =~ ^[23][0-9][0-9]$ ]]; then
		report resolved web "$text" "answers $code"
	else
		report unreachable web "$text" "the address answers $code"
	fi
}

# --- resolving a whole document -----------------------------------------------------

# Prints one line per citation of the document given as the first argument and leaves
# the three counts in count_resolved, count_unreachable and count_failed. The same
# citation written twice is one citation.
resolve_document() {
	local document=$1
	local -a matches
	local match path rest ident table column digit ref url dated

	count_resolved=0
	count_unreachable=0
	count_failed=0

	mapfile -t matches < <(grep -oE "$literal_pattern" <<<"$document" | sort -u)
	for match in "${matches[@]}"; do
		rest=${match#\(\`}
		path=${rest%%\`, \`*}
		ident=${rest#*\`, \`\"}
		ident=${ident%\"\`)}
		resolve_literal "$match" "$path" "$ident"
	done

	mapfile -t matches < <(grep -oE "$code_pattern" <<<"$document" | sort -u)
	for match in "${matches[@]}"; do
		rest=${match#\(\`}
		path=${rest%%\`, \`*}
		ident=${rest#*\`, \`}
		ident=${ident%\`)}
		resolve_code "$match" "$path" "$ident"
	done

	mapfile -t matches < <(grep -oE "$column_pattern" <<<"$document" | sort -u)
	for match in "${matches[@]}"; do
		rest=${match#the \`}
		column=${rest%%\`*}
		table=${rest##*column of \`}
		table=${table%\`}
		resolve_column "$match" "$column" "$table"
	done

	mapfile -t matches < <(grep -oE "$table_pattern" <<<"$document" | sort -u)
	for match in "${matches[@]}"; do
		table=${match#the \`}
		table=${table%\` table}
		resolve_table "$match" "$table"
	done

	mapfile -t matches < <(grep -oE "$experiment_pattern" <<<"$document" | tr 'E' 'e' | sed -E 's/[^0-9]+$//' | sort -u)
	for match in "${matches[@]}"; do
		digit=${match##* }
		resolve_experiment "$match" "$digit"
	done

	mapfile -t matches < <(grep -oE "$dispatch_pattern" <<<"$document" | sort -u)
	for match in "${matches[@]}"; do
		ref=$(without_trailing_punctuation "$match")
		resolve_dispatch "$ref"
	done

	mapfile -t matches < <(grep -oE "$web_pattern" <<<"$document" | sort -u)
	for match in "${matches[@]}"; do
		if [[ "$match" == *' (read '* ]]; then
			dated=yes
			url=${match%% (read *}
		else
			dated=no
			url=$match
			url=$(without_trailing_punctuation "$url")
		fi
		resolve_web "$match" "$url" "$dated"
	done

	if ((count_resolved + count_unreachable + count_failed == 0)); then
		report failed document 'the whole document' \
			'it carries no citation at all, and an empty set of citations is no evidence'
	fi
}

summary() {
	printf 'citations: %d resolved, %d unreachable, %d failed\n' \
		"$count_resolved" "$count_unreachable" "$count_failed"
}

# --- the negative control -----------------------------------------------------------

# Every refusal this script claims, one citation each. None of these may resolve.
broken_fixture() {
	cat <<'EOF'
A file that does not hold the identifier it is cited with:
(`packages/daemon/internal/record/store.go`, `record.Postgres.NoSuchMethodOfThisStore`).
A path the repository does not hold:
(`packages/daemon/internal/record/no_such_file.go`, `record.Postgres.Enqueue`).
An identifier the file holds only as a string, cited in the code form:
(`packages/daemon/internal/api/handoff.go`, `api.HANDOFF_STALE_ASSIGNMENT`).
A table no migration creates: the `no_such_table_of_infra_1546` table.
A column no migration declares: the `no_such_column_of_infra_1546` column of `outbox`.
A dispatch reference that does not read:
dispatch://INFRA-1546/artifact/no-such-document-of-infra-1546
A web address with no read date beside it: https://docs.temporal.io/evaluate/development-production-features
An address that answers 404: https://docs.temporal.io/no-such-page-of-infra-1546 (read 2026-11-07)
EOF
}

readonly broken_fixture_citations=8

# Citations of this repository that resolve today, in each form the grammar allows.
well_formed_fixture() {
	cat <<'EOF'
Code: (`packages/daemon/internal/record/store.go`, `record.Postgres.Enqueue`).
Literal: (`packages/daemon/internal/api/handoff.go`, `"HANDOFF_STALE_ASSIGNMENT"`).
Stored state: the `lease_token` column of `outbox`, and the `pending_task_deliveries` table.
Dispatch: dispatch://INFRA-1546.
Web: https://docs.temporal.io/evaluate/development-production-features (read 2026-11-07)
EOF
}

self_test() {
	local failures=0

	printf 'self-test, the broken half: every line below must be a refusal\n\n'
	resolve_document "$(broken_fixture)"
	summary
	if ((count_failed != broken_fixture_citations || count_resolved != 0)); then
		printf 'self-test: expected %d refusals and no resolved citation, got %d refusals, %d resolved and %d unreachable\n' \
			"$broken_fixture_citations" "$count_failed" "$count_resolved" "$count_unreachable" >&2
		failures=$((failures + 1))
	else
		printf 'self-test: all %d broken citations refused\n' "$broken_fixture_citations"
	fi

	printf '\nself-test, a document with no citation at all: it must be refused\n\n'
	resolve_document ''
	summary
	if ((count_failed != 1)); then
		printf 'self-test: a document with no citation was not refused\n' >&2
		failures=$((failures + 1))
	else
		printf 'self-test: a document with no citation refused\n'
	fi

	printf '\nself-test, the well-formed half: every line below must resolve\n\n'
	resolve_document "$(well_formed_fixture)"
	summary
	if ((count_failed != 0)); then
		printf 'self-test: %d well-formed citations did not resolve\n' "$count_failed" >&2
		failures=$((failures + 1))
	fi

	printf '\n'
	if ((failures > 0)); then
		printf 'self-test: %d of 3 checks did not hold\n' "$failures" >&2
		return 1
	fi
	printf 'self-test: the resolver refuses every broken form and resolves every well-formed one\n'
	return 0
}

# --- the live answer ----------------------------------------------------------------

check_answer() {
	local document status=0
	document=$(dispatch doc-read --ref "$answer_ref" 2>&1) || status=$?
	if ((status != 0)); then
		printf 'check-citations: the answer does not exist yet. Reading %s failed:\n' "$answer_ref" >&2
		printf '%s\n' "$document" >&2
		printf 'check-citations: there is nothing to resolve until that document is written. Run this script with --self-test to exercise the resolver meanwhile.\n' >&2
		return 1
	fi

	resolve_document "$document"
	summary
	((count_failed == 0))
}

main() {
	case "${1:-}" in
	'') check_answer ;;
	--self-test) self_test ;;
	*)
		printf 'usage: check-citations.sh [--self-test]\n' >&2
		exit 2
		;;
	esac
}

main "$@"

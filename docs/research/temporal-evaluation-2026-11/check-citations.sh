#!/usr/bin/env bash
#
# check-citations.sh resolves every citation the two documents of INFRA-1546 carry:
#
#   the answer   dispatch://INFRA-1546/artifact/temporal-evaluation-md
#   the plan     dispatch://INFRA-1546/artifact/implementation-plan-md
#
#   bash docs/research/temporal-evaluation-2026-11/check-citations.sh
#   bash docs/research/temporal-evaluation-2026-11/check-citations.sh --self-test
#
# It reads both of them live from Dispatch, so it checks what a reader reads rather
# than a copy. It prints one line per citation, naming in its third column the document
# that citation came from, and ends with one summary over both documents together:
#
#   citations: N resolved, M unreachable, 0 failed
#
# It runs from the repository root, or from anywhere else: it finds its own directory
# and the repository root from there, and it writes nothing.
#
# The grammar it implements, which both documents follow (the directory's README lists
# the kinds of citation; this header is where their grammar is written):
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
#                code or any other token that is no declaration. Its path is read under
#                the code form's rule, so no absolute path and no path holding "..": a
#                citation reads a file of this repository or it is refused.
#   Stored state the `lease_token` column of `outbox`, or the `outbox` table.
#                A table resolves on a create table of that name in a *.up.sql file
#                under packages/daemon/internal/store/migrations; a column resolves
#                when a migration creates that table with the column or alters the
#                table to add it.
#   Experiment   experiment 1 .. experiment 18. It resolves when that experiment's
#                recorded output is committed beside this script, and it counts as a
#                citation, so a section whose only evidence is a measurement is not a
#                section with no citation.
#   Figure       Any number a document attributes to a numbered experiment: the 92 of
#                "Experiment 1 drove ... 92 history events". It resolves when that text,
#                as written, is a number of its own in what that experiment's committed
#                transcript measured: no digit and no decimal point stands immediately
#                before or after it there, so a figure of 1.8 is not resolved by a
#                timestamp reading 51.864119017 and a figure of 0.16 is not resolved by
#                a cost reading 0.164294. Only the transcript's measured lines resolve a
#                figure. Every transcript opens with a header of a few lines and then a
#                line of dashes, and only what stands below that first all-dashes line
#                is read, so the recording time, the commit the run was made from and
#                the run id in the header resolve nothing, and a number that appears in
#                the header alone fails. A figure quoted from a run nobody committed
#                fails, and so does a figure written 1,015 where the transcript prints
#                1015.
#                The scope a number is read in is one table cell, and one sentence
#                within it: cells are separated by ` | ` in the rendered text, and a
#                sentence ends at ". ", "; ", "? " or the line end. A scope never runs
#                across a ` | `, so the verdict table's experiment column never lends
#                its experiment to the line counts in the next column. In a scope
#                naming experiment N, every numeric token is a figure except: the
#                experiment numbers themselves; a token in a code span; a token
#                immediately after a code span, after an optional comma, which is how
#                both documents write a Legion line count (`internal/workflow`, 8,158);
#                a token in a dispatch:// reference or a web address; a token inside a
#                YYYY-MM-DD date; and a single digit. A trailing comma, full stop or
#                semicolon is the sentence's, not the number's. Where a scope names
#                more than one experiment, any of their transcripts holding the text
#                resolves the figure.
#   Dispatch     A bare dispatch:// reference, resolved by `dispatch read --ref`.
#   Web          An https:// address immediately followed by ` (read YYYY-MM-DD)`. A
#                404 or a 410 fails. Any other answer prints unreachable and does not
#                fail the run. Of the exits curl itself can end on, exactly two are
#                lenient and print unreachable: 28, the request timed out, and 7,
#                nothing accepted the connection. Those two are the third party's
#                uptime, and a merge must not wait on that. Every other non-zero curl
#                exit fails, 6 among them, which is the host not resolving: a name that
#                resolves nowhere is a citation of nothing, not a site briefly down.
#
# It exits non-zero on: a file that does not hold the identifier it is cited with; an
# identifier cited in the code form that the file holds only as a string; a table no
# migration creates; a dispatch:// reference that does not read; a number a document
# attributes to an experiment that the experiment's committed transcript does not
# measure below its header; a citation whose path leaves the repository; a web address
# with no read date beside it; an address answering 404 or 410, or one curl ends on an
# exit other than a timeout and a connection nothing accepted; and a document with no
# citation at all, because an empty set of citations is no evidence.
# That last rule holds per document: each of the two must carry evidence of its own.
# A document that does not exist, or that Dispatch will not read, is refused by name
# before any citation of either document is resolved.
#
# --self-test runs the resolver over a fixture of deliberately broken citations held in
# this script and over a set of well-formed ones, so a tester can show the check
# refuses what it claims to refuse without editing a live document.

set -o errexit -o nounset -o pipefail

# The documents this script checks: the label every line of theirs carries, and the
# reference it is read from. The two arrays are read in step.
readonly -a document_labels=(
	answer
	plan
)
readonly -a document_refs=(
	'dispatch://INFRA-1546/artifact/temporal-evaluation-md'
	'dispatch://INFRA-1546/artifact/implementation-plan-md'
)
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
# The document whose citations are being resolved, printed in every line below.
# resolve_document sets it.
document_label=''

reset_counts() {
	count_resolved=0
	count_unreachable=0
	count_failed=0
}

report() {
	local status=$1 kind=$2 text=$3 detail=${4:-}
	case "$status" in
	resolved) count_resolved=$((count_resolved + 1)) ;;
	unreachable) count_unreachable=$((count_unreachable + 1)) ;;
	failed) count_failed=$((count_failed + 1)) ;;
	esac
	if [[ -n "$detail" ]]; then
		printf '%-11s %-7s %-10s %s: %s\n' "$status" "$kind" "$document_label" "$text" "$detail"
	else
		printf '%-11s %-7s %-10s %s\n' "$status" "$kind" "$document_label" "$text"
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
	if [[ "$path" = /* || "$path" == *..* ]]; then
		report failed literal "$text" "$path is no repository-rooted path"
		return
	fi
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

# Prints one record per number a scope of the document attributes to an experiment:
#
#   <the experiment numbers of the scope, comma separated>\t<the number>\t<the scope>
#
# The scoping rule is the one the Figure paragraph at the head of this script states.
# The same number attributed to the same experiments twice is one record.
# shellcheck disable=SC2016  # the backticks below are markdown, not substitution
experiment_figures() {
	awk '
	function blank(n,   s) {
		s = ""
		while (n-- > 0) s = s " "
		return s
	}

	# Blanks out what the rule excludes by its position: a code span, a numeric token
	# immediately after one, a dispatch:// reference or a web address, and a date.
	# Blanking rather than deleting keeps every other token where it was.
	function sanitize(s,   out, i, j, k, m, n, c) {
		out = ""
		i = 1
		n = length(s)
		while (i <= n) {
			c = substr(s, i, 1)
			if (substr(s, i) ~ /^(https?|dispatch):\/\//) {
				j = i
				while (j <= n && substr(s, j, 1) != " " && substr(s, j, 1) != "\t") j++
				out = out blank(j - i)
				i = j
				continue
			}
			if (c == "`") {
				j = i + 1
				while (j <= n && substr(s, j, 1) != "`") j++
				k = (j <= n) ? j + 1 : j
				out = out blank(k - i)
				i = k
				m = i
				if (substr(s, m, 1) == ",") m++
				while (substr(s, m, 1) == " ") m++
				if (substr(s, m, 1) ~ /[0-9]/) {
					k = m
					while (k <= n && substr(s, k, 1) ~ /[0-9.,]/) k++
					out = out substr(s, i, m - i) blank(k - m)
					i = k
				}
				continue
			}
			if (substr(s, i) ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]/) {
				out = out blank(10)
				i += 10
				continue
			}
			out = out c
			i++
		}
		return out
	}

	function figures(text,   numbers, named, pos, hit, t, n, i, j, k, token, own) {
		sub(/^[ \t]+/, "", text)
		sub(/[ \t]+$/, "", text)

		# Every experiment the scope names, possessive or not, 1 to 18.
		numbers = ""
		pos = 1
		while (match(substr(text, pos), /[Ee]xperiment (1[0-8]|[1-9])([^0-9]|$)/) > 0) {
			hit = substr(text, pos + RSTART - 1, RLENGTH)
			sub(/^[Ee]xperiment /, "", hit)
			sub(/[^0-9]$/, "", hit)
			if (index("," numbers ",", "," hit ",") == 0)
				numbers = numbers (numbers == "" ? "" : ",") hit
			pos = pos + RSTART + RLENGTH - 2
		}
		if (numbers == "") return
		split(numbers, named, ",")

		t = sanitize(text)
		n = length(t)
		i = 1
		while (i <= n) {
			if (substr(t, i, 1) !~ /[0-9]/) { i++; continue }
			j = i
			while (j <= n && substr(t, j, 1) ~ /[0-9.,]/) j++
			token = substr(t, i, j - i)
			i = j
			# The sentence punctuation a number carries is no part of it.
			while (token ~ /[.,;]$/) token = substr(token, 1, length(token) - 1)
			if (length(token) < 2) continue
			own = 0
			for (k in named) if (named[k] == token) own = 1
			if (own) continue
			if (seen[numbers "\t" token]++) continue
			print numbers "\t" token "\t" text
		}
	}

	{
		cells = split($0, cell, / \| /)
		for (ci = 1; ci <= cells; ci++) {
			sentences = split(cell[ci], sentence, /\. |; |\? /)
			for (si = 1; si <= sentences; si++) figures(sentence[si])
		}
	}
	' <<<"$1"
}

# What a transcript measured: every line below the first line of dashes, which is the
# rule under the header carrying the recording time, the commit the run was made from
# and the run id. A figure is read against these lines alone, so a number the header
# holds and no measurement holds resolves nothing.
measured_lines() {
	awk 'body { print; next } /^-+$/ { body = 1 }' <"$1"
}

resolve_experiment_figure() {
	local numbers=$1 figure=$2 scope=$3 text detail digit transcript escaped pattern
	local -a recorded=() searched=()
	text="the figure $figure of experiment ${numbers//,/ or }"
	# The figure as a number of its own: neither a digit nor a decimal point touches it.
	escaped=${figure//./\\.}
	pattern='(^|[^0-9.])'"$escaped"'([^0-9.]|$)'
	for digit in ${numbers//,/ }; do
		mapfile -t recorded < <(find "$script_dir" -maxdepth 1 -type f -name "experiment-${digit}-*.txt" -size +0 | sort)
		for transcript in "${recorded[@]}"; do
			searched+=("${transcript##*/}")
			if grep -qE -- "$pattern" <(measured_lines "$transcript"); then
				report resolved figure "$text" "measured in ${transcript##*/}"
				return
			fi
		done
	done
	if ((${#searched[@]} == 0)); then
		detail="no recorded output of experiment ${numbers//,/ or } is committed beside this script, so the figure is quoted from a run nobody committed"
	elif ((${#searched[@]} == 1)); then
		detail="${searched[0]} measures no $figure as a number of its own"
	else
		detail="none of ${searched[*]} measures $figure as a number of its own"
	fi
	report failed figure "$text" "$detail; the scope reads: $scope"
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
	case "$status" in
	0) ;;
	28)
		report unreachable web "$text" "the address did not answer within ${web_timeout}s (curl exit 28)"
		return
		;;
	7)
		report unreachable web "$text" 'nothing accepted the connection (curl exit 7)'
		return
		;;
	6)
		report failed web "$text" 'curl cannot resolve the host of this address (curl exit 6)'
		return
		;;
	*)
		report failed web "$text" "curl could not read the address (curl exit $status)"
		return
		;;
	esac
	if [[ "$code" == 404 || "$code" == 410 ]]; then
		report failed web "$text" "the address answers $code"
	elif [[ "$code" =~ ^[23][0-9][0-9]$ ]]; then
		report resolved web "$text" "answers $code"
	else
		report unreachable web "$text" "the address answers $code"
	fi
}

# --- resolving a whole document -----------------------------------------------------

# Prints one line per citation of the document given as the second argument, each line
# naming the document by the label given as the first, and adds what it finds to the
# three counts, which the caller resets. The same citation written twice is one
# citation, and a document carrying no citation at all is itself a refusal, so a
# document says nothing about what another one carries.
resolve_document() {
	local document=$2 before
	local -a matches
	local match path rest ident table column digit ref url dated numbers figure scope

	document_label=$1
	before=$((count_resolved + count_unreachable + count_failed))

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

	mapfile -t matches < <(experiment_figures "$document")
	for match in "${matches[@]}"; do
		IFS=$'\t' read -r numbers figure scope <<<"$match"
		resolve_experiment_figure "$numbers" "$figure" "$scope"
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

	if ((count_resolved + count_unreachable + count_failed == before)); then
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
An address whose host no name server can resolve: https://no-such-host.invalid/ (read 2026-11-07)
A literal citation reading outside the repository:
(`../../../../etc/passwd`, `"root"`).
EOF
}

readonly broken_fixture_citations=10

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

# The experiment-figure rule, in one fixture: three figures experiment 1's committed
# transcript holds, one it does not, one that its transcript holds only inside a longer
# number, one that stands in its transcript's header alone, and one number in the cell
# beside an experiment mention, which the cell boundary keeps out of the check. The
# table row is the line that fails if a scope is ever allowed to run across a ` | `:
# experiment 1's transcript never prints 1,294 as written (its one count of Legion's
# lines is 1294, unseparated), and 1,294 is Legion's own count of the table and the
# engine. The 0.16 is the line that fails if a figure is ever resolved by a longer
# number it is a part of: experiment 2's transcript prints the thousand-gate cost as
# 0.164294 and never as 0.16. The 42 is the line that fails if a figure is ever
# resolved against a transcript's header rather than against what it measured:
# experiment 1's header records the run at 22:39:42Z, and nothing it measured is 42.
experiment_figure_fixture() {
	cat <<'EOF'
Experiment 1 drove one issue in a 27-line workflow function over a data table, 12 signals and 92 history events, as its committed transcript holds it.
Experiment 1 recorded 999999 history events, a figure no committed transcript of it holds.
Experiment 2 priced a thousand gates standing open at 0.16 dollars of Active storage.
Experiment 1 moved through 42 phases, a number only the recording time in its transcript's header holds.
| Phase transitions | the measurement is experiment 1 | 1,294 lines of table and engine |
EOF
}

# Two resolved citations of the experiments themselves and three resolved figures, 27,
# 12 and 92; three refusals, 999999, 0.16 and the header's 42. The 1,294 of the last
# cell is no figure of experiment 1, so it is neither.
readonly experiment_figure_fixture_resolved=5
readonly experiment_figure_fixture_failed=3

self_test() {
	local failures=0

	printf 'self-test, the broken half: every line below must be a refusal, the address whose host resolves nowhere and the literal citation reading outside the repository among them\n\n'
	reset_counts
	resolve_document broken "$(broken_fixture)"
	summary
	if ((count_failed != broken_fixture_citations || count_resolved != 0)); then
		printf 'self-test: expected %d refusals and no resolved citation, got %d refusals, %d resolved and %d unreachable\n' \
			"$broken_fixture_citations" "$count_failed" "$count_resolved" "$count_unreachable" >&2
		failures=$((failures + 1))
	else
		printf 'self-test: all %d broken citations refused, among them the address whose host resolves nowhere, which is a refusal and not an unreachable address, and the literal citation whose path leaves the repository\n' "$broken_fixture_citations"
	fi

	printf '\nself-test, a document with no citation at all: it must be refused\n\n'
	reset_counts
	resolve_document empty ''
	summary
	if ((count_failed != 1)); then
		printf 'self-test: a document with no citation was not refused\n' >&2
		failures=$((failures + 1))
	else
		printf 'self-test: a document with no citation refused\n'
	fi

	printf '\nself-test, the well-formed half: every line below must resolve\n\n'
	reset_counts
	resolve_document well-formed "$(well_formed_fixture)"
	summary
	if ((count_failed != 0)); then
		printf 'self-test: %d well-formed citations did not resolve\n' "$count_failed" >&2
		failures=$((failures + 1))
	fi

	printf '\nself-test, the experiment-figure rule: a figure the transcript measured resolves, one it does not is refused, one it holds only inside a longer number is refused, one its header alone holds is refused, and a number in the cell beside an experiment mention is not checked at all\n\n'
	reset_counts
	resolve_document figures "$(experiment_figure_fixture)"
	summary
	if ((count_resolved != experiment_figure_fixture_resolved || count_failed != experiment_figure_fixture_failed)); then
		printf 'self-test: expected %d resolved and %d refused of the experiment-figure fixture, got %d resolved, %d refused and %d unreachable\n' \
			"$experiment_figure_fixture_resolved" "$experiment_figure_fixture_failed" \
			"$count_resolved" "$count_failed" "$count_unreachable" >&2
		failures=$((failures + 1))
	else
		printf 'self-test: the measured figure resolved, the figure outside the transcript, the figure only inside a longer number and the figure standing in the header alone were refused, and the number in the next cell was not checked\n'
	fi

	printf '\n'
	if ((failures > 0)); then
		printf 'self-test: %d of 4 checks did not hold\n' "$failures" >&2
		return 1
	fi
	printf 'self-test: the resolver refuses every broken form and resolves every well-formed one\n'
	return 0
}

# --- the live documents -------------------------------------------------------------

# Reads both documents before resolving either, so a document Dispatch will not read is
# refused on its own rather than after a screenful of another document's citations.
check_documents() {
	local -a texts=()
	local i label ref text status
	for i in "${!document_refs[@]}"; do
		label=${document_labels[i]}
		ref=${document_refs[i]}
		status=0
		text=$(dispatch doc-read --ref "$ref" 2>&1) || status=$?
		if ((status != 0)); then
			printf 'check-citations: the %s does not read. Reading %s failed:\n' "$label" "$ref" >&2
			printf '%s\n' "$text" >&2
			printf 'check-citations: there is nothing to resolve until that document is written. Run this script with --self-test to exercise the resolver meanwhile.\n' >&2
			return 1
		fi
		texts[i]=$text
	done

	reset_counts
	for i in "${!document_refs[@]}"; do
		resolve_document "${document_labels[i]}" "${texts[i]}"
	done
	summary
	((count_failed == 0))
}

main() {
	case "${1:-}" in
	'') check_documents ;;
	--self-test) self_test ;;
	*)
		printf 'usage: check-citations.sh [--self-test]\n' >&2
		exit 2
		;;
	esac
}

main "$@"

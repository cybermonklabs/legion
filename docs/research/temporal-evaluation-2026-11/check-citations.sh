#!/usr/bin/env bash
#
# check-citations.sh resolves the citations of the two documents of INFRA-1546 that
# the grammar below reads, and prints how many resolved. It does not read every
# citation those documents carry. What it does not read stands under "What this gate
# does not read" below, which is this header's statement of the coverage table the
# script holds once: --coverage prints that table, the directory's README states it in
# the same words, and the self-test refuses a run where either prose has dropped a line
# of it, so an account of these gaps written from memory fails in this directory rather
# than in review. What the count covers is the grammar below and nothing wider, and
# whether a claim is missing its citation altogether is the reviewer's reading, as the
# issue's Verification document says in its own words.
# The two documents:
#
#   the answer   dispatch://INFRA-1546/artifact/temporal-evaluation-md
#   the plan     dispatch://INFRA-1546/artifact/implementation-plan-md
#
#   bash docs/research/temporal-evaluation-2026-11/check-citations.sh
#   bash docs/research/temporal-evaluation-2026-11/check-citations.sh --self-test
#   bash docs/research/temporal-evaluation-2026-11/check-citations.sh --coverage
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
#                `record.Postgres.Enqueue` (`packages/daemon/internal/record/store.go`)
#                Two forms, one rule. Either a repository-rooted path in a code span, a
#                comma and a space, then a dotted identifier in a code span, all in
#                parentheses; or the identifier in a code span first and its path in a
#                code span in parentheses after it. Both resolve when that file holds
#                the identifier's last dotted element as a Go declaration: a function,
#                a method, a type, a var, a const, a member of a grouped var, const or
#                type, a struct field, or a method in an interface. A declaration and
#                not a use: a name a function body assigns, returns or calls is no
#                declaration of it, so `record.err` resolves against no file whose
#                functions merely use err. No relative path and no "same file", either
#                way round: an absolute path and a path holding ".." are refused in
#                both forms. Both print the same resolved and failed lines, so the
#                output does not say which way round a citation was written.
#                An identifier in a code span with no path beside it in either form is
#                read by neither rule and reported by neither line. The documents write
#                Temporal's `workflow.Sleep` and their own file names the same way, so
#                nothing in such a span says whether a file was meant.
#   Literal      (`packages/daemon/internal/api/handoff.go`, `"HANDOFF_STALE_ASSIGNMENT"`)
#                The first of those two shapes with the second span double-quoted. It
#                resolves on the exact quoted text being in that file. This is the form
#                for a refusal code or any other token that is no declaration. Its path
#                is read under the code form's rule, so no absolute path and no path
#                holding "..": a citation reads a file of this repository or it is
#                refused.
#   Stored state the `lease_token` column of `outbox`, or the `outbox` table.
#                A table resolves on a create table of that name in a *.up.sql file
#                under packages/daemon/internal/store/migrations; a column resolves
#                when a migration creates that table with the column or alters the
#                table to add it.
#   Experiment   experiment N, for any N this grammar names: a number written after
#                "experiment" or "experiments", or listed in the plural that word
#                opens, names an experiment, and a number standing anywhere else in a
#                sentence names none. That is the whole of what makes a number the
#                name of an experiment, and the size of the number is no part of it,
#                so the plain numbers both documents write everywhere are read as
#                figures or as nothing. It resolves when that experiment's recorded
#                output is committed beside this script, and it is refused when none
#                is, whether its number stands above the highest transcript this
#                directory holds or in a gap below that one: what makes an experiment
#                this directory's is its transcript, committed beside this script, so
#                the count can never read as a pass for a figure nobody looked at.
#                Either way it counts as a citation, so a section whose only evidence
#                is a measurement is not a section with no citation. The transcripts
#                are read from the directory at every run, so a twenty-second
#                experiment recorded beside this script is resolved by this one with
#                no edit here.
#                The plural names more than one experiment, and each one it names is a
#                citation of its own, resolved against its own transcript:
#                "experiments N and M" and "experiments N, M and O" name each number
#                listed, however many, where the word "and" closes the list; a comma
#                alone closes nothing, so the 12 of "experiment 1, 12 signals" stays a
#                figure. "experiments N to M" names every experiment from N to M,
#                above the highest transcript committed here as below it. A number
#                below 1 names none, there being no zeroth experiment.
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
#                its experiment to the line counts in the next column. A scope names an
#                experiment by the singular "experiment N" and by each plural form the
#                Experiment paragraph lists, so the 19 and the 20 of "experiments 19
#                and 20" are that scope's experiment numbers and a figure of neither.
#                In a scope naming an experiment, every numeric token is a figure
#                except what "What this gate does not read" below lists, which was
#                walked out of the branches of sanitize and figures and which
#                --coverage prints. A trailing comma, full stop or semicolon is the
#                sentence's, not the number's. Where a scope names more than one
#                experiment, any of their transcripts holding the text resolves the
#                figure.
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
# What this gate does not read, line for line as the coverage table below holds it:
# --coverage prints these lines, the directory's README states them in the same words,
# and the self-test fails naming any line either prose home has dropped. The first list
# is the citation kinds no rule here reads at all; the second is every exclusion that
# keeps a numeric token of a scope naming an experiment from being read as a figure,
# walked branch by branch out of sanitize and figures rather than remembered.
#
# The citation kinds no rule of this gate reads:
#
#   A dotted identifier with no path beside it, in either order, is read by no rule
#   here, because the documents write Temporal's `workflow.Sleep` and their own file
#   names in the same shape and nothing in such a span says whether a file was meant.
#
#   A span mixing a code citation with another kind is read only in the part that
#   matches.
#
#   A stored-state citation in any other words than the `x` column of `y` and the `y`
#   table is read by no rule, so the `lease_token` column of the `outbox` table resolves
#   as the table alone and a table or column name holding anything but letters, digits
#   and the underscore is read by neither shape.
#
#   A dispatch:// reference is read only as far as its first character outside letters,
#   digits, the dot, the underscore, the slash, @, # and -, so what is checked is the
#   part before that character.
#
#   An http:// address is read by no rule, the web rule reading https:// alone, though
#   the figure rule keeps the numbers of both out of the scopes it reads.
#
#   A citation broken across a line end is read by no rule, every rule here reading one
#   line at a time.
#
#   A scope never crosses a table cell, so a figure in a table cell is not attributed to
#   the experiment the neighbouring cell names.
#
# In a scope naming an experiment, what keeps a numeric token from being read as a
# figure:
#
#   The experiment numbers of the scope itself are no figures of it, the 19 and the 20
#   of experiments 19 and 20 among them.
#
#   A numeric token inside a code span is no figure.
#
#   A numeric token immediately after a code span, after an optional comma, is no
#   figure, which is how both documents write a Legion line count, so the 1,294 lines of
#   the phase table and its engine and the 2,751 of the outbox's three files that the
#   answer's size argument rests on are read by no rule here and were checked by hand,
#   by the tester and again by the reviewer.
#
#   A numeric token inside a dispatch:// reference or a web address is no figure,
#   http:// as well as https://.
#
#   A numeric token inside a YYYY-MM-DD date is no figure.
#
#   A numeric token of a single digit is no figure, which includes the counts the
#   comparison at the heart of the answer rests on, two side effects against one: those
#   were checked by hand, by the tester and again by the reviewer, rather than by any
#   rule here.
#
#   Every numeric token after an unterminated code span is no figure, because a backtick
#   with no closing backtick after it blanks the rest of that line.
#
# It exits non-zero on: a file that does not hold the identifier it is cited with; an
# identifier cited in the code form that the file holds only as a string; a table no
# migration creates; a dispatch:// reference that does not read; an experiment named
# with no transcript committed beside this script, its number above the highest
# transcript committed there or in a gap below it alike; a number of two characters or
# more that a scope naming an experiment attributes to it, outside every exclusion the
# coverage table lists, that the experiment's committed transcript does not measure
# below its header; a citation whose path leaves the repository; a web address
# with no read date beside it; an address answering 404 or 410; an address whose host no
# name server resolves; and a document with no citation at all, because an empty set of
# citations is no evidence.
# That last rule holds per document: each of the two must carry evidence of its own.
# Every other answer an address gives, and every transport fault other than a host no
# name server resolves, prints unreachable and does not fail the run, because a merge
# must not wait on a third party's uptime.
# A document that does not exist, or that Dispatch will not read, is refused by name
# before any citation of either document is resolved.
#
# --self-test runs the resolver over a fixture of deliberately broken citations held in
# this script and over a set of well-formed ones, so a tester can show the check
# refuses what the fixtures hold without editing a live document, and reads the coverage
# table against this header and the README, so neither prose can drift from it.

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
# This script and the directory's README: the two prose homes of the coverage table,
# which the self-test reads the table against.
script_path="$script_dir/${BASH_SOURCE[0]##*/}"
readonly script_path
readonly coverage_readme="$script_dir/README.md"
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
	code_reversed_pattern='`[A-Za-z0-9_.]+` \(`[^`]+`\)' \
	literal_pattern='\(`[^`]+`, `"[^"]*"`\)' \
	column_pattern='the `[A-Za-z0-9_]+` column of `[A-Za-z0-9_]+`' \
	table_pattern='the `[A-Za-z0-9_]+` table' \
	dispatch_pattern='dispatch://[A-Za-z0-9._/@#-]+' \
	web_pattern='https://[^][:space:]`")<>]+( \(read [0-9]{4}-[0-9]{2}-[0-9]{2}\))?'

# The highest-numbered transcript committed beside this script, 0 when none is. It
# bounds no citation: the experiment rule and the figure rule read the transcripts
# themselves, one experiment at a time, so a transcript recorded beside this script is
# read by both with no edit here, and a number no transcript answers is refused rather
# than passed over. It is read for one thing only: a run made where no transcript of
# any experiment this grammar can name stands beside this script is refused by name,
# rather than one refusal at a time of every experiment the documents cite.
highest_experiment_recorded() {
	local file base number highest=0
	for file in "$script_dir"/experiment-[0-9]*-*.txt; do
		[[ -s "$file" ]] || continue
		base=${file##*/}
		number=${base#experiment-}
		number=${number%%-*}
		[[ "$number" =~ ^[0-9]+$ ]] || continue
		if ((number > highest)); then highest=$number; fi
	done
	printf '%s\n' "$highest"
}

experiment_max=$(highest_experiment_recorded)
readonly experiment_max
((experiment_max > 0)) ||
	die "no experiment transcript experiment-N-*.txt stands beside this script in $script_dir, so no experiment and no figure of one can be resolved"

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
# member of a grouped var, const or type, a struct field, or a method in an interface.
#
# The first three stand at the left margin and are read there. The last three are
# indented, and an indented line is read only inside a body that holds declarations
# and nothing else: a grouped var, const or type opened by "var (", "const (" or
# "type (" and closed by a ")" at the margin, and the body of a struct or an interface
# opened by its "type NAME struct {" or "type NAME interface {" and closed by a "}" at
# the margin. Inside such a body every line one tab in declares what it names, which a
# line of a function body does not: err is declared by none of the files whose
# functions assign it, return it or pass it on, so (`.../store.go`, `record.err`) is
# refused rather than resolved against a file that merely uses the name.
#
# One tab is the depth gofmt gives a member of a body opened at the margin, so a name
# deeper than that -- a key of a composite literal in a grouped var, a field of a
# struct nested in a struct -- is no declaration this reads. A declaration may name
# more than one at once, as "A, B string" and "a, b = 1, 2" do, and each name it lists
# is declared by it.
go_declares() {
	local file=$1 name=$2
	awk -v name="$name" '
	BEGIN {
		ident = "[A-Za-z_][A-Za-z0-9_]*"
		# The names a declaration may list before the one cited.
		also = "(" ident "[ \t]*,[ \t]*)*"
		func_decl = "^func[ \t]+" name "[ \t]*[([]"
		method_decl = "^func[ \t]*\\([^)]*\\)[ \t]*" name "[ \t]*[([]"
		top_decl = "^(type|var|const)[ \t]+" also name "([ \t,[]|$)"
		member_decl = "^\t" also name "([ \t,([]|$)"
		opens_group = "^(type|var|const)[ \t]*\\([ \t]*$"
		# A body this reads is opened at the end of its line and closed at the
		# margin, so a "type X struct{}" that opens and closes on one line opens
		# none and leaves no body open over the lines below it.
		opens_body = "^type[ \t]+" ident "(\\[[^]]*\\])?[ \t]+(struct|interface)[ \t]*[{][ \t]*$"
	}
	$0 ~ func_decl || $0 ~ method_decl || $0 ~ top_decl { found = 1; exit }
	closer != "" && substr($0, 1, 1) == closer { closer = ""; next }
	closer != "" && $0 ~ member_decl { found = 1; exit }
	$0 ~ opens_group { closer = ")"; next }
	$0 ~ opens_body { closer = "}" }
	END { exit found ? 0 : 1 }
	' "$file"
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

# The awk function both experiment rules read text with: every experiment a stretch of
# text names, as a comma-separated list in the order they are named, empty when it
# names none.
#
#   experiment N               N alone
#   experiments N and M        N and M
#   experiments N, M and O     each number listed, however many, when the word "and"
#                              closes the list: a comma alone closes nothing, so the
#                              12 of "experiment 1, 12 signals" is no experiment
#   experiments N to M         every experiment from N to M
#
# What names an experiment is this grammar and nothing else, so a plain number of
# ordinary prose names none, and the size of the number is no part of it: a number
# above the highest transcript committed beside this script names an experiment this
# directory has no transcript for, which the caller refuses rather than passes over.
# A number below 1 names none, there being no zeroth experiment.
# shellcheck disable=SC2016  # this is awk source, not shell
readonly experiments_named_awk='
	function experiments_named(text,   out, pos, rest, plural, hit, j, list, count, ended, sep, hi, m, k) {
		out = ""
		pos = 1
		while (match(substr(text, pos), /[Ee]xperiments? [0-9]+/) > 0) {
			rest = substr(text, pos + RSTART - 1)
			pos = pos + RSTART + RLENGTH - 1
			plural = (rest ~ /^[Ee]xperiments /)
			hit = substr(rest, 1, RLENGTH)
			j = RLENGTH + 1
			sub(/^[Ee]xperiments? /, "", hit)
			count = 1
			list[1] = hit + 0
			ended = 1
			if (plural && match(substr(rest, j), /^ to [0-9]+/) > 0) {
				hi = substr(rest, j + 4, RLENGTH - 4) + 0
				j = j + RLENGTH
				for (m = list[1] + 1; m <= hi; m++) list[++count] = m
			} else if (plural) {
				ended = 0
				while (match(substr(rest, j), /^(, and | and |, )[0-9]+/) > 0) {
					sep = substr(rest, j, RLENGTH)
					j = j + RLENGTH
					ended = (sep ~ / and /)
					sub(/^[^0-9]+/, "", sep)
					list[++count] = sep + 0
				}
			}
			if (!ended) count = 1
			for (k = 1; k <= count; k++) {
				if (list[k] < 1) continue
				if (index("," out ",", "," list[k] ",") > 0) continue
				out = out (out == "" ? "" : ",") list[k]
			}
		}
		return out
	}
'

# Every experiment the document names, one number per line, each named once, whether
# it was named on its own or in a plural naming several.
experiment_mentions() {
	awk "$experiments_named_awk"'
	{
		named = experiments_named($0)
		if (named == "") next
		count = split(named, each, ",")
		for (i = 1; i <= count; i++) if (!seen[each[i]]++) print each[i]
	}
	' <<<"$1"
}

# Prints one record per number a scope of the document attributes to an experiment:
#
#   <the experiment numbers of the scope, comma separated>\t<the number>\t<the scope>
#
# The scoping rule is the one the Figure paragraph at the head of this script states.
# The same number attributed to the same experiments twice is one record.
# shellcheck disable=SC2016  # the backticks below are markdown, not substitution
experiment_figures() {
	awk "$experiments_named_awk"'
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

	function figures(text,   numbers, named, t, n, i, j, k, token, own) {
		sub(/^[ \t]+/, "", text)
		sub(/[ \t]+$/, "", text)

		# Every experiment the scope names, singular or plural, possessive or not.
		numbers = experiments_named(text)
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

# The verdict a curl exit status earns, written as the verdict, a tab and the message
# that says what happened. Exit 6 is the one transport failure this gate refuses: an
# address whose host no name server resolves is a citation of nothing. Every other
# transport fault is unreachable, because a merge must not wait on a third party's
# uptime, and every message names the exit curl ended on, so a reader can tell a
# timeout from a connection nothing accepted from any other fault.
web_transport_verdict() {
	local status=$1
	case "$status" in
	6) printf 'failed\tcurl cannot resolve the host of this address (curl exit 6)\n' ;;
	28) printf 'unreachable\tthe address did not answer within %ss (curl exit 28)\n' "$web_timeout" ;;
	7) printf 'unreachable\tnothing accepted the connection (curl exit 7)\n' ;;
	*) printf 'unreachable\tcurl could not read the address (curl exit %s)\n' "$status" ;;
	esac
}

resolve_web() {
	local text=$1 url=$2 dated=$3 code status=0 verdict
	if [[ "$dated" != yes ]]; then
		# shellcheck disable=SC2016  # the backticks are markdown, not substitution
		report failed web "$text" 'no read date beside it; write it as `<address> (read YYYY-MM-DD)`'
		return
	fi
	code=$(curl --silent --location --max-time "$web_timeout" \
		--output /dev/null --write-out '%{http_code}' -- "$url" 2>/dev/null) || status=$?
	if ((status != 0)); then
		verdict=$(web_transport_verdict "$status")
		report "${verdict%%$'\t'*}" web "$text" "${verdict#*$'\t'}"
		return
	fi
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
	local match path rest ident table column number ref url dated numbers figure scope

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

	# The same citation the other way round: the identifier first, its path in
	# parentheses after it. It is read by the same resolver, so it prints the same
	# lines and is refused for the same reasons.
	mapfile -t matches < <(grep -oE "$code_reversed_pattern" <<<"$document" | sort -u)
	for match in "${matches[@]}"; do
		ident=${match#\`}
		ident=${ident%%\`*}
		path=${match#*\` \(\`}
		path=${path%\`)}
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

	mapfile -t matches < <(experiment_mentions "$document" | sort -n)
	for number in "${matches[@]}"; do
		resolve_experiment "experiment $number" "$number"
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

# --- what this gate does not read ---------------------------------------------------

# The gate's own account of its gaps, held here once and printed by --coverage. The
# header of this script and the directory's README state these lines in the same words,
# and the self-test fails naming any line either prose home has dropped, so an account
# of the gaps written from memory fails in this directory rather than in review.
#
# Each entry is the canonical phrase both prose homes must hold, a tab, and the line
# --coverage prints, which holds that phrase.

# The citation kinds no rule of this gate reads at all, read out of the patterns
# resolve_document greps for above.
readonly -a coverage_kinds=(
	$'a dotted identifier with no path beside it\tA dotted identifier with no path beside it, in either order, is read by no rule here, because the documents write Temporal\'s `workflow.Sleep` and their own file names in the same shape and nothing in such a span says whether a file was meant.'
	$'a span mixing a code citation with another kind\tA span mixing a code citation with another kind is read only in the part that matches.'
	$'a stored-state citation in any other words\tA stored-state citation in any other words than the `x` column of `y` and the `y` table is read by no rule, so the `lease_token` column of the `outbox` table resolves as the table alone and a table or column name holding anything but letters, digits and the underscore is read by neither shape.'
	$'a dispatch:// reference is read only as far as its first character outside\tA dispatch:// reference is read only as far as its first character outside letters, digits, the dot, the underscore, the slash, @, # and -, so what is checked is the part before that character.'
	$'an http:// address is read by no rule\tAn http:// address is read by no rule, the web rule reading https:// alone, though the figure rule keeps the numbers of both out of the scopes it reads.'
	$'a citation broken across a line end\tA citation broken across a line end is read by no rule, every rule here reading one line at a time.'
	$'a figure in a table cell\tA scope never crosses a table cell, so a figure in a table cell is not attributed to the experiment the neighbouring cell names.'
)

# Every exclusion that keeps a numeric token of a scope naming an experiment from being
# read as a figure, walked branch by branch out of the two functions that hold them:
# the address branch, the code-span branch with the token after it and the date branch
# of sanitize, and the own-number branch and the length branch of figures.
readonly -a coverage_figures=(
	$'the experiment numbers of the scope itself\tThe experiment numbers of the scope itself are no figures of it, the 19 and the 20 of experiments 19 and 20 among them.'
	$'a numeric token inside a code span\tA numeric token inside a code span is no figure.'
	$'a numeric token immediately after a code span\tA numeric token immediately after a code span, after an optional comma, is no figure, which is how both documents write a Legion line count, so the 1,294 lines of the phase table and its engine and the 2,751 of the outbox\'s three files that the answer\'s size argument rests on are read by no rule here and were checked by hand, by the tester and again by the reviewer.'
	$'a numeric token inside a dispatch:// reference or a web address\tA numeric token inside a dispatch:// reference or a web address is no figure, http:// as well as https://.'
	$'a numeric token inside a YYYY-MM-DD date\tA numeric token inside a YYYY-MM-DD date is no figure.'
	$'a numeric token of a single digit\tA numeric token of a single digit is no figure, which includes the counts the comparison at the heart of the answer rests on, two side effects against one: those were checked by hand, by the tester and again by the reviewer, rather than by any rule here.'
	$'every numeric token after an unterminated code span\tEvery numeric token after an unterminated code span is no figure, because a backtick with no closing backtick after it blanks the rest of that line.'
)

# The coverage table, one line per thing this gate does not read.
coverage_report() {
	local entry
	printf 'coverage: what this gate does not read. Its header and the directory'"'"'s README state these lines in the same words, and --self-test refuses a run where either has dropped one.\n'
	printf '\nThe citation kinds no rule of this gate reads:\n\n'
	for entry in "${coverage_kinds[@]}"; do
		printf '  %s\n' "${entry#*$'\t'}"
	done
	printf '\nIn a scope naming an experiment, what keeps a numeric token from being read as a figure:\n\n'
	for entry in "${coverage_figures[@]}"; do
		printf '  %s\n' "${entry#*$'\t'}"
	done
}

# This script's header as one lowercased line, its comment markers and line breaks
# taken out, so a phrase is found whether the header wrapped it or not. The header is
# the comment block at the top of this file and nothing below it, so the coverage table
# cannot stand in for the prose it is checked against.
header_prose() {
	awk '!/^#/ { exit } { sub(/^#[ \t]?/, ""); gsub(/[ \t]+/, " "); printf "%s ", tolower($0) }' \
		"$script_path"
}

# The directory's README the same way.
readme_prose() {
	awk '{ gsub(/[ \t]+/, " "); printf "%s ", tolower($0) }' "$coverage_readme"
}

# Every line the coverage table prints standing in both prose homes. Prints to standard
# error the phrase of each line that does not, and where it is missing.
coverage_stated_in_prose() {
	local printed header readme entry phrase line missing=0
	if [[ ! -f "$coverage_readme" ]]; then
		printf 'self-test: %s is not there, so the coverage table has one prose home and not two\n' \
			"$coverage_readme" >&2
		return 1
	fi
	printed=$(coverage_report)
	header=$(header_prose)
	readme=$(readme_prose)
	for entry in "${coverage_kinds[@]}" "${coverage_figures[@]}"; do
		# Every comparison below is in lower case, the prose capitalising a phrase
		# that opens a sentence and the table writing it as it reads in the line.
		phrase=${entry%%$'\t'*}
		phrase=${phrase,,}
		line=${entry#*$'\t'}
		if [[ "$printed" != *"$line"* ]]; then
			printf 'self-test: --coverage does not print the coverage line "%s"\n' "$line" >&2
			missing=$((missing + 1))
			continue
		fi
		if [[ "${line,,}" != *"$phrase"* ]]; then
			printf 'self-test: the coverage line "%s" does not hold the phrase "%s" it is checked by\n' \
				"$line" "$phrase" >&2
			missing=$((missing + 1))
			continue
		fi
		if [[ "$header" != *"$phrase"* ]]; then
			printf 'self-test: the coverage line "%s" stands nowhere in this script'"'"'s header\n' \
				"$phrase" >&2
			missing=$((missing + 1))
		fi
		if [[ "$readme" != *"$phrase"* ]]; then
			printf 'self-test: the coverage line "%s" stands nowhere in %s\n' \
				"$phrase" "${coverage_readme##*/}" >&2
			missing=$((missing + 1))
		fi
	done
	((missing == 0))
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
A code citation written the other way round, whose file does not hold the identifier:
`record.Postgres.NoSuchMethodOfThisStore` (`packages/daemon/internal/record/store.go`).
A code citation written the other way round, reading outside the repository:
`record.Postgres.Enqueue` (`../../../../etc/passwd`).
A name the cited file only uses, which its functions assign and pass on and no
declaration of that file declares:
(`packages/daemon/internal/record/store.go`, `record.err`).
A word that begins indented lines of the cited file without declaring anything:
(`packages/daemon/internal/record/store.go`, `record.return`).
EOF
}

readonly broken_fixture_citations=14

# Citations of this repository that resolve today, in each form the grammar allows.
well_formed_fixture() {
	cat <<'EOF'
Code: (`packages/daemon/internal/record/store.go`, `record.Postgres.Enqueue`).
Code, the other way round: `supervise.Delivery` (`packages/daemon/internal/supervise/delivery.go`).
Literal: (`packages/daemon/internal/api/handoff.go`, `"HANDOFF_STALE_ASSIGNMENT"`).
Stored state: the `lease_token` column of `outbox`, and the `pending_task_deliveries` table.
Dispatch: dispatch://INFRA-1546.
Web: https://docs.temporal.io/evaluate/development-production-features (read 2026-11-07)
Code, a struct field: (`packages/daemon/internal/record/record.go`, `record.Issue.Generation`).
Code, a member of a grouped const: (`packages/daemon/internal/daemon/outbox.go`, `daemon.outboxSettleTimeout`).
Code, a method in an interface: (`packages/daemon/internal/record/record.go`, `record.Store.Gate`).
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

# The two rules widened after a review found this check reading experiments 1 to 18
# alone, and widened again after a later review found a number above the highest
# transcript passed over rather than refused; one fixture holds both: a figure of
# experiment 21, which the first range read as a figure of no experiment and left
# unchecked; a figure of 21 its transcript does not measure; two scopes naming
# experiments in the plural, one with "and" and one with a range; and a sentence
# naming experiment 25, of which this directory holds no transcript. That last line
# is the one that fails if a number above the highest transcript committed here is
# ever passed over again: experiment 25 is refused for the transcript nobody
# committed, and so is the 999999 it carries, where a reading that dropped the 25
# would take the sentence to name no experiment and would print no line of either, so
# the summary would read as a pass over a figure nothing was read for.
# The plural lines are the ones that fail if a scope naming several experiments is ever
# read as naming none: the 19 and the 20 would then be numbers of whatever experiment
# the sentence named, and the 11 would be a figure of nothing and go unchecked. They
# are also what fails if the numbers of a plural are ever read as figures of it: each
# of 19, 20, 16 and 18 stands in its own transcript inside a task queue name, so a
# reading that took them for figures would resolve and the count below would not hold.
experiments_named_fixture() {
	cat <<'EOF'
Experiment 21 handed a task token of 144 bytes to a process that holds a working copy, as its committed transcript holds it.
Experiment 21 handed a token of 4096 bytes, a figure no committed transcript of it holds.
Experiments 19 and 20 left the handout Started on attempt 1, the longer of them across 11 readings.
Experiments 16 to 18 each have a transcript committed beside this script, which the range names one by one.
Experiment 25 recorded 999999 history events.
EOF
}

# Six experiments named and resolved -- 21, named twice and counted once; 19 and 20 of
# the plural; and 16, 17 and 18, which the range names although it writes only two of
# them -- and two figures, the 144 of experiment 21 and the 11 of experiment 20. Three
# refusals: the 4096 no transcript of experiment 21 measures, experiment 25, which no
# transcript committed here records, and the 999999 that sentence attributes to it.
readonly experiments_named_fixture_resolved=8
readonly experiments_named_fixture_failed=3

# Every curl exit status the verdict rule is driven over, and the verdict each must
# earn: a status, a tab and the verdict. Exit 6, the host no name server resolves, is
# the one refusal; the timeout of 28, the connection nothing accepted of 7 and every
# other transport fault alike are unreachable. 56, a transfer broken in the middle, is
# the status three github.com addresses the plan cites ended on while GitHub was
# faulting, which failed a merge gate this table now holds to unreachable; 35 is a TLS
# handshake that did not finish, 52 an empty answer and 18 a transfer that stopped
# short. The table is read with no address and no network: the rule is a function of
# the exit status alone.
readonly -a web_transport_cases=(
	$'6\tfailed'
	$'28\tunreachable'
	$'7\tunreachable'
	$'56\tunreachable'
	$'35\tunreachable'
	$'52\tunreachable'
	$'18\tunreachable'
)

# Drives web_transport_verdict over that table, printing the verdict and the message of
# each exit. Prints to standard error every exit that earned the wrong verdict and
# every message that does not name the exit curl ended on.
web_transport_verdicts_hold() {
	local entry status want got verdict message wrong=0
	for entry in "${web_transport_cases[@]}"; do
		status=${entry%%$'\t'*}
		want=${entry#*$'\t'}
		got=$(web_transport_verdict "$status")
		verdict=${got%%$'\t'*}
		message=${got#*$'\t'}
		printf '%-11s %-7s %-10s curl exit %s: %s\n' "$verdict" web transport "$status" "$message"
		if [[ "$verdict" != "$want" ]]; then
			printf 'self-test: curl exit %s must be %s and is %s: %s\n' \
				"$status" "$want" "$verdict" "$message" >&2
			wrong=$((wrong + 1))
			continue
		fi
		if [[ "$message" != *"curl exit $status"* ]]; then
			printf 'self-test: the %s message of curl exit %s does not name the exit: %s\n' \
				"$verdict" "$status" "$message" >&2
			wrong=$((wrong + 1))
		fi
	done
	((wrong == 0))
}

self_test() {
	local failures=0

	printf 'self-test, the broken half: every line below must be a refusal, the address whose host resolves nowhere, the literal citation reading outside the repository, the two code citations written the other way round, one naming a file that does not hold its identifier and one reading outside the repository, and the two names the cited file only uses, among them\n\n'
	reset_counts
	resolve_document broken "$(broken_fixture)"
	summary
	if ((count_failed != broken_fixture_citations || count_resolved != 0)); then
		printf 'self-test: expected %d refusals and no resolved citation, got %d refusals, %d resolved and %d unreachable\n' \
			"$broken_fixture_citations" "$count_failed" "$count_resolved" "$count_unreachable" >&2
		failures=$((failures + 1))
	else
		printf 'self-test: all %d broken citations refused, among them the address whose host resolves nowhere, which is a refusal and not an unreachable address, the literal citation whose path leaves the repository, the two citations written identifier first, which are refused on the same two rules as the ones written path first, and the two names the cited file only uses, which no declaration of it declares\n' "$broken_fixture_citations"
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

	printf '\nself-test, the well-formed half: every line below must resolve, the code citation written identifier first among them\n\n'
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

	printf '\nself-test, every experiment a document can name and the plural that names them: a figure of experiment 21 resolves, one its transcript does not measure is refused, a scope naming experiments in the plural names each of them, so each is a citation of its own and none of their numbers is read as a figure, and a sentence naming experiment 25, which this directory holds no transcript of, is refused for the experiment and for the figure it carries rather than passed over\n\n'
	reset_counts
	resolve_document named "$(experiments_named_fixture)"
	summary
	if ((count_resolved != experiments_named_fixture_resolved || count_failed != experiments_named_fixture_failed)); then
		printf 'self-test: expected %d resolved and %d refused of the plural fixture, got %d resolved, %d refused and %d unreachable\n' \
			"$experiments_named_fixture_resolved" "$experiments_named_fixture_failed" \
			"$count_resolved" "$count_failed" "$count_unreachable" >&2
		failures=$((failures + 1))
	else
		printf 'self-test: experiment 21 and the figure its transcript measures resolved, the figure it never measured was refused, the six experiments of two plurals were each named, each resolved and each kept out of the figures of its scope, and experiment 25, which stands above every transcript committed here, was refused together with the 999999 that sentence attributes to it\n'
	fi

	printf '\nself-test, the verdict a curl exit earns, driven over a table of exit statuses with no address and no network: the host no name server resolves is the one transport failure this gate refuses, and every other transport fault is unreachable, so a third party'"'"'s bad minute cannot turn a merge red\n\n'
	if web_transport_verdicts_hold; then
		printf 'self-test: each of the %d curl exits of the table above earned its verdict and named itself in its message: exit 6, the host no name server resolves, refused, and the timeout of 28, the connection nothing accepted of 7 and every other transport fault, the 56 of a transfer broken in the middle among them, unreachable\n' \
			"${#web_transport_cases[@]}"
	else
		failures=$((failures + 1))
	fi

	printf '\nself-test, the coverage table against the prose that states it: every line --coverage prints must stand in this script'"'"'s header and in the directory'"'"'s README, so neither account of what this gate does not read can be rewritten from memory and lose a line\n\n'
	coverage_report
	printf '\n'
	if coverage_stated_in_prose; then
		printf 'self-test: all %d lines of the coverage table above stand in this script'"'"'s header and in %s, which proves the two prose accounts of what this gate does not read are the table'"'"'s own list and not a remembered one: drop a line from either and this check names it and the prose home it is missing from\n' \
			"$((${#coverage_kinds[@]} + ${#coverage_figures[@]}))" "${coverage_readme##*/}"
	else
		failures=$((failures + 1))
	fi

	printf '\n'
	if ((failures > 0)); then
		printf 'self-test: %d of 7 checks did not hold\n' "$failures" >&2
		return 1
	fi
	printf 'self-test: every broken citation these fixtures hold was refused and every well-formed one resolved, and every curl exit of the transport table earned its verdict, the host no name server resolves alone being a refusal, which is what the fixtures prove and no more; --coverage lists the citation kinds and the figures no rule here reads at all\n'
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
	--coverage) coverage_report ;;
	*)
		printf 'usage: check-citations.sh [--self-test|--coverage]\n' >&2
		exit 2
		;;
	esac
}

main "$@"

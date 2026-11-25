#!/usr/bin/env bash
# This repository is public. The private repository Legion is deployed from, its private internal
# hosts, and the company that runs it are named nowhere in docs, skills, role prompts, CHANGELOGs,
# code comments, tests, fixtures or file names, since the published site renders text from all of
# them. This check fails on any text line naming one, and on any file path naming the first two or
# a term of the fifth rule:
#
#   1. the deployment repository — "agent", then "c", with nothing or one separator between them,
#      in any case, standing as a token of its own: no letter or digit on either side, and a
#      percent-encoded byte before it counts as a separator too. A separator is a "-" or "_", a
#      run of spaces or no-break spaces, a typographic dash, or a percent-encoded byte. That is the
#      repository's name, its Dispatch project key and every issue key under it, and the name
#      inside a session id between underscores, but not a camelCase or snake_case identifier that
#      merely starts with those letters (agentCursor, AgentConversation, subagentContext,
#      AGENT_COMPOSER_SELECTOR, agent-composer);
#   2. the company — its two words joined, or split by one separator or a ".", in any case (its
#      hostnames included); and its first word capitalized or in capitals, as a whole word or
#      before a "_", which is the company's proper noun or a constant named after it wherever it
#      stands, at the start of a sentence or a heading too. The lowercase English word names nothing
#      and passes;
#   3. a private host below `.internal.`, in any case. A reserved example host (`<name>` then
#      `.internal.example`, optionally `.com`, `.net` or `.org`) passes, and so does the NATS
#      client's own `this` property of that name in the one bundle that vendors it
#      (packages/claude-envoy/dist/envoy-channel.js, which reaches this check on main and through
#      packages/claude-envoy/scripts/verify-build.sh, never in a pull request's diff), and nowhere
#      else. A namespace passes too: a name holding a capital letter, then a segment spelled
#      exactly `Internal` and a full stop, as a Lean module or function or an OCaml value path
#      has, one label in front included (`Std.Internal.` passes). That is the one shape a host and
#      a namespace share, so a host written that way passes with it. So does Lean's own option
#      `internal.cmdlineSnapshots` under its `Lean` namespace, and no near name.
#      A run with PRIVATE_NAME_BARE_HOSTS=yes, which check-pr-text.sh asks for over a pull
#      request's own text, also refuses a host that ends in `.internal`, as Dispatch's results
#      line refuses it, since that text is pasted where no reader stands between. Each label of
#      such a host starts with a letter or a digit, and `.internal` ends it when what follows is
#      any character but a letter, a digit, a `.` or a `_` (a port's `:`, a path's `/`, a blank,
#      a quote, a bracket, a hyphen), a full stop with no letter or digit after it, one `_` with
#      no letter or digit after it or two or more `_` whatever follows, or the end of the line. So
#      `nats.internal_url` passes and `db.internal__url` is refused. The namespace above passes
#      there too, with one label in front and its full stop (`Std.Internal.`); with no full stop
#      after it, or with two or more labels in front at a sentence's end, it is refused there.
#      Only the GCE metadata server's and Docker's host name of that shape pass, with no label
#      joined in front: a letter or digit then a `-` joins one, so `x-metadata.google.internal`
#      is refused, while a `-` after no letter or digit, and any `_`, leave them whole
#      (`-metadata.google.internal:80` and `x_metadata.google.internal` pass; no DNS name holds a
#      `_`). A run over a tree reads no such ending: the tree holds those two names and its tests'
#      own hosts of that shape, which name no private network. The bare reading is the CI check's
#      alone, on purpose: this script's port, .github/private-names.toml, which runs where
#      something names it (the port harness, release-push.sh's leak guard, and a deployment that
#      names it as its leak guard's rule file), does not read it, since it would refuse agents'
#      own writes;
#   4. a Tailscale MagicDNS host — a DNS label then `.ts.net`, in any case, with no letter or
#      digit after it. A reserved tailnet name (`example-tailnet`, under any machine name of its
#      own in front) passes;
#   5. a term of PRIVATE_NAME_TERMS: the names of the private deployment that this public file
#      cannot spell, kept in the Actions secret of that name. The variable holds one term a line,
#      each with the blanks around it (a carriage return among them) dropped, and a line left
#      empty is dropped too, since grep reads an empty pattern as one that matches every line.
#      Each term is matched as given, in any case, anywhere in a line or a path, so a term that
#      holds a "/" is found across a path's parts: term_pattern turns it into a pattern for
#      grep -E -i in the run's locale, which counts a letter's case counterparts, and adds the few
#      characters whose case mapping folds to a letter that grep does not count beside it (a
#      dotless i or a capital I with a dot for an i, a Kelvin sign for a k, a long s for an s, and
#      so on). A full case folding that turns one character into two (a sharp s into "ss") is not
#      applied. The allowed hosts of rules 3 and 4 are exceptions to those rules alone, never to a
#      term. A line of the form `key:<KEY>` (capitals and digits) is no term: it names one Dispatch
#      project of the deployment's own, whose issue keys the sixth rule refuses;
#   6. an issue key of one of those projects: the key, a "-" and a number, with no letter or digit
#      on either side (`(^|[^[:alnum:]])<KEY>-[0-9]+([^[:alnum:]]|$)`, case-sensitive, so the bare
#      word and a lowercase slug pass, and a key beside a "_", which a GNU "\b" would pass, is
#      refused). `LEGION` is never such a project, so Legion's own `LEGION-<n>` keys always pass.
#      A run that reads this rule with terms in the list and no `key:` line fails with exit 2, since
#      it would report a clean result over no project at all. A Dispatch link is no private name
#      (the operator decided so): no rule refuses a link for being one; a name, term or issue key
#      in it is still refused.
#
# Rule 6 reads new text only: the tree keeps the keys from before it, which the operator's rule
# ("Stop new leaks only") leaves in place. PRIVATE_NAME_RULES picks the rules a run reads: `names`
# (the default) reads rules 1 to 5, and `keys` reads rule 6 alone, which check-pr-text.sh asks for
# over a pull request's new text (its header says which text, and from when).
#
# One matching engine reads everything: GNU grep, in the run's locale. The repository's contents
# and a named path's are read by the same grep over the same file list, a file's path by grep -z,
# and the masking below by grep too, each with the same patterns, so a spelling one of them finds,
# all of them find, and every hit they find is masked the same way.
#
# The jobs that run this script with that secret run no dependency code: each checks out the
# repository, or downloads what another job built, and runs this script and system tools alone.
# With the variable unset or empty (no term left after the dropping above) the fifth rule cannot
# run, and GITHUB_EVENT_NAME decides what follows. On a pull_request or a pull_request_review event,
# which GitHub runs with no secret for a fork's pull request, and with no event at all (a run
# outside Actions), the check prints a warning that only the first four rules ran and keeps their
# exit; a keys run with no `key:` line left checks nothing, and its warning and its last line say
# so, with exit 0. On any other event (a merge_group, a push, a workflow_dispatch), where a job can
# always map the secret, it fails with exit 2 before it scans anything.
#
# With no arguments it scans the repository: every file git tracks, a file under an ignored path
# included, plus every untracked file git does not ignore (git ls-files -z), so a new file is
# caught before it is added. With arguments it scans each named directory or file instead,
# tracked or not (find -print0); docs-site.yaml's private-names job passes it the built site
# (docs/site/dist), whose generated reference pages and copied media captions exist only after
# the build, and check-pr-text.sh a pull request's title, body, branch name, commits (each one's
# message, author and committer), issue comments, reviews and review comments. The first two
# rules and the fifth also read every listed path, directories included, as git and find print it
# unquoted. The contents read are the listed regular files: a symbolic link is not followed,
# binary files are skipped (grep -I), and so are lockfiles (bun.lock, go.sum, go.work.sum), whose
# integrity hashes are random text. Every list and hit is a NUL-terminated record, so a file name
# holding a ":", a tab or a newline stays whole from its listing to its report. Every hit is
# printed as its file and line, never its text, and each file and line counts once, however its
# path prints. A path part that the first two rules match is printed as <name>, and so is every
# part an occurrence of a term touches, overlapping occurrences included, a run of such parts as
# one <name>; a path that still holds a term after that is printed as <name> whole. Every other
# line that carries a path, a named target or another command's message is masked the same way,
# so the log repeats no name and no fragment of a term, and each annotation's file and message are
# escaped as GitHub reads a workflow command (a "%", a newline, and in the file a ":" or ","). The
# only non-zero exits are named hits (1), and a target or search that failed or a term list the
# event requires and the job did not map (2).
#
# The rules below are spelled so that no line of this file is refused by them: a line a rule's raw
# pattern matches, such as a reserved placeholder host, always clears that rule's own allow-list
# too, which is why the check needs no exception for itself.
#
# Run from anywhere: .github/scripts/check-private-names.sh [path...]
# CI runs it over the repository in ci-job-lint.yaml's private-names job, which ci.yaml's lint job
# calls; over the built site in docs-site.yaml's private-names job, which ci.yaml's docs job and
# docs-pages.yaml's build job call; over the packed Claude Code plugin's dist/ and legion/ through
# .github/actions/private-names-packed-plugin, in ci-job-claude-envoy.yaml's private-names job and
# release.yaml's claude_envoy_names job; over each release's rendered changelog entries, the file
# its publish job writes into the GitHub release's body, through
# .github/actions/private-names-release-notes, in release-notes-names.yaml's job (which
# release.yaml's legion_names, pi_envoy_names and pi_legion_names and release-envoy-listener.yaml's
# release_names call) and release.yaml's claude_envoy_names job;
# over a pull request's title, body, branch name, commits
# (each one's message, author and committer), issue comments, reviews and review comments through
# check-pr-text.sh (ci.yaml's pr-title job at each push to a pull request that is not a draft, when
# a draft is marked ready and on the merge queue's group, and pr-title.yaml when the pull request
# is edited); and in its tests, in ci.yaml's test job and its private-names-port job, which run
# with placeholder terms of their own and no secret. Each step that runs it with the secret maps
# PRIVATE_NAME_TERMS.
set -euo pipefail
# No tracing: a traced run (`bash -x`, or xtrace in SHELLOPTS) would print each term, and the forms
# a test or a match makes of it, wherever the lines below expand one. This comes before the first.
set +x

# A relative path argument is read from the caller's directory, so it is prefixed with that
# directory's path from the repository root (or its absolute path, outside the repository) before
# the cd below. The path is never rewritten: the kernel follows its symlinks and `..` parts as the
# caller's shell would, and a name starting with "-" gets a "./" so find does not read an option.
# Each directory is read with an "x" after pwd's newline, since a command substitution drops every
# trailing newline, a directory name's own included.
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd -P && printf x)
repo_root=${repo_root%$'\n'x}
caller=$(pwd -P && printf x)
caller=${caller%$'\n'x}
case $caller in
  "$repo_root") prefix= ;;
  "$repo_root"/*) prefix=${caller#"$repo_root"/}/ ;;
  *) prefix=$caller/ ;;
esac
args=()
for arg in "$@"; do
  case $arg in
    '' | /*) args+=("$arg") ;;
    -*) args+=("${prefix:-./}$arg") ;;
    *) args+=("$prefix$arg") ;;
  esac
done
set -- "${args[@]}"
cd "$repo_root"

encoded_byte='%[[:xdigit:]]{2}'
dashes='‐|‑|‒|–|—|―|−|﹘|﹣|－'
no_break_space=$'\xc2\xa0'
figure_space=$'\xe2\x80\x87'
narrow_no_break_space=$'\xe2\x80\xaf'
spaces="( |$no_break_space|$figure_space|$narrow_no_break_space)+"
# One separator between the two parts of a name: a hyphen or underscore, a run of spaces or
# no-break spaces, a typographic dash pasted from a document, or a percent-encoded byte.
separator="[-_]|$spaces|$encoded_byte|$dashes"
repository_rule="(^|[^[:alnum:]]|$encoded_byte)agent($separator)?c([^[:alnum:]]|\$)"
company_rule="t""rajectory($separator|[.])?labs"
proper_noun_rule="(^|[^[:alnum:]])(T""rajectory|T""RAJECTORY)([^[:alnum:]]|\$)"
private_host_rule='[[:alnum:]-]+[.]internal[.][[:alnum:].-]+'
# Whole matches of the host rule that name no private host: a reserved example host (a trailing
# full stop is the sentence's), and the NATS client's property in the bundle that vendors it.
allowed_host='[[:alnum:]-]+[.]internal[.]example([.](com|net|org))?[.]*'
vendored_nats='packages/claude-envoy/dist/envoy-channel.js'
vendored_property='this[.]internal[.][[:alnum:]_]+'
# Two whole matches of the host rule that name a Lean name, read case-sensitively, since their
# case is their shape: a name holding a capital letter, then a segment spelled exactly Internal;
# and Lean's own option `internal.cmdlineSnapshots` under its Lean namespace, with at most one
# method after it, as src/Lean/Elab/Frontend.lean of leanprover/lean4 sets it.
namespace_host='[^[:alnum:]]?[[:alnum:]-]*[[:upper:]][[:alnum:]-]*[.]Internal[.]'
lean_option='Lean[.]internal[.]cmdlineSnapshots([.][[:alnum:]]+)?'
# A Tailscale MagicDNS host: one DNS label immediately before the literal, with no letter or digit
# right after it (a GNU "\b" word boundary would miss a host that sits next to a "_").
tailnet_host_rule='[[:alnum:]-]+[.]ts[.]net([^[:alnum:]]|$)'
# A whole match of the tailnet rule that names no private tailnet: the reserved example tailnet
# name (the match is only its last label, so a machine name in front of it passes the same way),
# with the one trailing character the rule itself may have captured.
allowed_tailnet_host='example-tailnet[.]ts[.]net[^[:alnum:]]?'

repository_label='names the private deployment repository or its Dispatch project key'
company_label='names the company'
private_host_label='names a private internal host'
tailnet_host_label='names a Tailscale MagicDNS host'
term_label="names a term of the deployment's own list (PRIVATE_NAME_TERMS)"
issue_key_label="names an issue key of the deployment's own projects (PRIVATE_NAME_TERMS key: lines)"
rules=${PRIVATE_NAME_RULES:-names}
case $rules in
  names | keys) ;;
  *)
    echo "check-private-names: PRIVATE_NAME_RULES must be names or keys" >&2
    exit 2
    ;;
esac
# The third rule's reading of a pull request's own text (PRIVATE_NAME_BARE_HOSTS=yes, the header
# says what it refuses): a host that ends in `.internal`, every label before it included, each
# starting with a letter or a digit. Its whole matches that name no private host: the two
# well-known names, with the trailing characters the rule may have captured. Neither is in the
# variables .github/private-names.toml ports, which read no such text.
bare_host_rule='([[:alnum:]][[:alnum:]-]*[.])*[[:alnum:]][[:alnum:]-]*[.]internal([^[:alnum:]._]|_+([^[:alnum:]]|$)|[.]([^[:alnum:]]|$)|$)'
allowed_bare_host='(metadata[.]google|host[.]docker)[.]internal([^[:alnum:].]|[.][^[:alnum:]]?|_+[^[:alnum:]]?)?'
bare_hosts=${PRIVATE_NAME_BARE_HOSTS:-no}
case $bare_hosts in
  yes | no) ;;
  *)
    echo "check-private-names: PRIVATE_NAME_BARE_HOSTS must be yes or no" >&2
    exit 2
    ;;
esac

lockfiles=(bun.lock go.sum go.work.sum)
targets=("$@")

# Every path list and hit below is a NUL-terminated record, so a file name holding a ":", a tab or
# a newline stays whole from its listing to its report.
matches=$(mktemp)
hits=$(mktemp)
paths=$(mktemp)
files=$(mktemp)
errors=$(mktemp)
patterns_file=$(mktemp)
# One file for each pattern a search reads alone (term_spans, and the sixth rule's key pattern), so
# no term or key reaches grep's arguments, which any process of the job can read.
pattern_dir=$(mktemp -d)
trap 'rm -f "$matches" "$hits" "$paths" "$files" "$errors" "$patterns_file"; rm -rf "$pattern_dir"' EXIT

# The characters whose case mapping folds to a letter but which grep's -i does not count beside
# it, each spelled as its UTF-8 bytes, since a Unicode normaliser rewrites three of them (the
# Kelvin, Angstrom and Ohm signs) to the letter they stand for.
dotless_i=$'\xc4\xb1'
dotted_capital_i=$'\xc4\xb0'
kelvin_sign=$'\xe2\x84\xaa'
long_s=$'\xc5\xbf'
a_ring=$'\xc3\xa5'
angstrom_sign=$'\xe2\x84\xab'
omega=$'\xcf\x89'
ohm_sign=$'\xe2\x84\xa6'
theta=$'\xce\xb8'
capital_theta_symbol=$'\xcf\xb4'
sharp_s=$'\xc3\x9f'
capital_sharp_s=$'\xe1\xba\x9e'

# term_pattern <term>: the term as an extended regular expression for grep -E -i: its own letters,
# each with grep's case counterparts, and beside a letter the characters above that fold to it (a
# capital I with a dot and a dotless i for an i, a Kelvin sign for a k, a long s for an s, an
# Angstrom sign for an a with a ring, an Ohm sign for an omega, a capital theta symbol for a theta
# and a capital sharp s for a sharp s); every other character stands for itself.
term_pattern() {
  local term=$1 char pattern='' i
  for ((i = 0; i < ${#term}; i++)); do
    char=${term:i:1}
    case ${char,,} in
      i | "$dotless_i") pattern+="[${char}i${dotless_i}${dotted_capital_i}]" ;;
      k) pattern+="[${char}k${kelvin_sign}]" ;;
      s | "$long_s") pattern+="[${char}s${long_s}]" ;;
      "$a_ring") pattern+="[${char}${a_ring}${angstrom_sign}]" ;;
      "$omega") pattern+="[${char}${omega}${ohm_sign}]" ;;
      "$theta") pattern+="[${char}${theta}${capital_theta_symbol}]" ;;
      "$sharp_s") pattern+="[${char}${sharp_s}${capital_sharp_s}]" ;;
      '.' | '[' | ']' | \\ | '(' | ')' | '*' | '+' | '?' | '{' | '}' | '|' | '^' | '$')
        pattern+="\\$char" ;;
      *) pattern+=$char ;;
    esac
  done
  printf '%s' "$pattern"
}

# The fifth rule's terms: each line of PRIVATE_NAME_TERMS with the blanks around it dropped, and
# none left empty (the header says why), each beside its pattern; and the sixth rule's projects,
# each line of the form `key:<KEY>`, LEGION never among them. They are read before this script
# prints anything, since every line below that carries a path or another command's message is
# masked with them. A byte-order mark is dropped wherever it is, and a no-break space counts as a
# blank at either end: [[:space:]] holds neither, and a term that kept one would match nothing
# while the count line still reported it read.
terms=()
term_patterns=()
keys=()
bom=$'\xef\xbb\xbf'
nbsp=$'\xc2\xa0'
while IFS= read -r term; do
  term=${term//"$bom"/}
  while :; do
    term=${term#"${term%%[![:space:]]*}"}
    term=${term%"${term##*[![:space:]]}"}
    case $term in
      "$nbsp"*) term=${term#"$nbsp"} ;;
      *"$nbsp") term=${term%"$nbsp"} ;;
      *) break ;;
    esac
  done
  if [[ $term =~ ^key:([A-Z][A-Z0-9]*)$ ]]; then
    if [ "${BASH_REMATCH[1]}" != LEGION ]; then keys+=("${BASH_REMATCH[1]}"); fi
  elif [ -n "$term" ]; then
    terms+=("$term")
    term_patterns+=("$(term_pattern "$term")")
  fi
done <<< "${PRIVATE_NAME_TERMS:-}"
# The sixth rule's pattern: any of the keys, a "-" and a number, with no letter or digit on either
# side (a GNU "\b" would pass a key beside a "_", as the tailnet rule's comment says).
key_pattern=''
key_file="$pattern_dir/key"
if [ ${#keys[@]} -gt 0 ]; then
  key_pattern="(^|[^[:alnum:]])($(IFS='|'; echo "${keys[*]}"))-[0-9]+([^[:alnum:]]|\$)"
  printf '%s\n' "$key_pattern" > "$key_file"
fi
for i in "${!term_patterns[@]}"; do printf '%s\n' "${term_patterns[i]}" > "$pattern_dir/term.$i"; done
# One line says how many terms and keys the run read, and never a term or a key: on a pull
# request's own run, the proof that the job holds the list. An empty list prints the warning
# below instead.
if [ ${#terms[@]} -gt 0 ]; then
  printf '%s\n' "${term_patterns[@]}" > "$patterns_file"
  if [ ${#terms[@]} -eq 1 ]; then read_count="1 term"; else read_count="${#terms[@]} terms"; fi
  if [ ${#keys[@]} -eq 1 ]; then
    read_count+=" and 1 issue key"
  elif [ ${#keys[@]} -gt 1 ]; then
    read_count+=" and ${#keys[@]} issue keys"
  fi
  echo "check-private-names: PRIVATE_NAME_TERMS: $read_count read"
fi

# named_parts <part...>: the 1-based index of each part the first two rules match, one a line, by
# the grep their searches run.
named_parts() {
  local record
  {
    printf '%s\0' "$@" | grep -z -n -E -i -e "$repository_rule" -e "$company_rule" || true
    printf '%s\0' "$@" | grep -z -n -E -e "$proper_noun_rule" || true
  } | while IFS= read -r -d '' record; do printf '%s\n' "${record%%:*}"; done
}

# names_term <text>: whether a term of the fifth rule, or an issue key of the sixth, is in text,
# by the grep its search runs.
names_term() {
  { [ ${#terms[@]} -gt 0 ] && grep -q -E -i -f "$patterns_file" <<< "$1"; } ||
    { [ -n "$key_pattern" ] && grep -q -E -f "$key_file" <<< "$1"; }
}

# byte_length <text>: text's length in bytes, the unit of grep's -b offsets.
byte_length() { (LC_ALL=C; printf '%s' "${#1}"); }

# term_spans <text>: one "start end" line, in bytes, for every occurrence of a term or an issue
# key in text, by the grep their searches run (a term in any case, a key as written). Each search
# starts again one character after the start of the occurrence before it, so an occurrence that
# overlaps another, of the same term or of another, has a line of its own.
term_spans() {
  local text=$1 pattern_file at found start match case_flag i
  local -a pattern_files=()
  for i in "${!term_patterns[@]}"; do pattern_files+=("$pattern_dir/term.$i"); done
  if [ -n "$key_pattern" ]; then pattern_files+=("$key_file"); fi
  for pattern_file in "${pattern_files[@]}"; do
    case_flag=-i
    if [ "$pattern_file" = "$key_file" ]; then case_flag=-E; fi
    at=0
    while found=$(grep -a -o -b -E "$case_flag" -f "$pattern_file" <<< "$(LC_ALL=C; printf '%s' "${text:at}")"); do
      found=${found%%$'\n'*}
      start=$((at + ${found%%:*}))
      match=${found#*:}
      printf '%s %s\n' "$start" "$((start + $(byte_length "$match")))"
      at=$((start + $(byte_length "${match:0:1}")))
    done
  done
}

# mask <text>: text, a path or a message that holds one, with each "/"-separated part that the
# first two rules match replaced by <name>, and each part an occurrence of a term touches, by
# byte offset and overlapping occurrences included, replaced too, a run of such parts by one
# <name>; so no fragment of a term is left. Every match is grep's, as the searches' are. A text
# that names_term still finds a term in after that is printed as <name> whole.
mask() {
  local IFS=/ masked_text i offset span_start span_end
  local -a parts=() masked=() starts=() ends=() touched=() kept=()
  read -r -d '' -a parts < <(printf '%s' "$1") || true
  masked=("${parts[@]}")
  if [ ${#parts[@]} -gt 0 ]; then
    while IFS= read -r i; do masked[i - 1]="<name>"; done < <(named_parts "${parts[@]}")
  fi
  if names_term "$1"; then
    offset=0
    for i in "${!parts[@]}"; do
      starts[i]=$offset
      ends[i]=$((offset + $(byte_length "${parts[i]}")))
      offset=$((ends[i] + 1))
    done
    while IFS=' ' read -r span_start span_end; do
      for i in "${!parts[@]}"; do
        if [ "${starts[i]}" -lt "$span_end" ] && [ "$span_start" -lt "${ends[i]}" ]; then
          touched[i]=1
        fi
      done
    done < <(term_spans "$1")
  fi
  for i in "${!masked[@]}"; do
    if [ -z "${touched[i]:-}" ]; then
      kept+=("${masked[i]}")
    elif [ "$i" -eq 0 ] || [ -z "${touched[i - 1]:-}" ]; then
      kept+=("<name>")
    fi
  done
  masked_text=${kept[*]}
  if names_term "$masked_text"; then masked_text="<name>"; fi
  printf '%s' "$masked_text"
}

for target in "${targets[@]}"; do
  if [ ! -e "$target" ]; then
    echo "check-private-names: $(mask "$target") does not exist, so there is nothing to scan there" >&2
    exit 2
  fi
done

if [ ${#terms[@]} -eq 0 ]; then
  case ${GITHUB_EVENT_NAME:-} in
    '' | pull_request | pull_request_review)
      if [ "$rules" = names ]; then
        echo "::warning::check-private-names: PRIVATE_NAME_TERMS is unset or empty, so only the" \
          "rules this script spells ran, and no term of the deployment's own list was checked" >&2
      elif [ ${#keys[@]} -eq 0 ]; then
        echo "::warning::check-private-names: PRIVATE_NAME_TERMS is unset or empty, so no issue" \
          "key of the deployment's own projects was checked" >&2
      fi
      ;;
    *)
      echo "::error::check-private-names: PRIVATE_NAME_TERMS is unset or empty on a" \
        "$GITHUB_EVENT_NAME run, which must check the deployment's own list: map" \
        "'PRIVATE_NAME_TERMS: \${{ secrets.PRIVATE_NAME_TERMS }}' into this step's env, and" \
        "write that Actions secret if the repository has none" >&2
      exit 2
      ;;
  esac
fi
# Rule 6 reads the `key:` lines of the list. A keys run under a list that holds terms and no `key:`
# line would check no project and report clean, so it fails instead. A list with no term at all is
# handled above.
if [ "$rules" = keys ] && [ ${#terms[@]} -gt 0 ] && [ ${#keys[@]} -eq 0 ]; then
  echo "::error::check-private-names: PRIVATE_NAME_TERMS holds no key: line, so the issue-key" \
    "rule has no project to read; add one key:<KEY> line for each of the deployment's own" \
    "Dispatch projects to that Actions secret" >&2
  exit 2
fi

# sanitized <command...>: runs the command with its error output masked line by line, since a
# command's message can carry a path that names something private; returns the command's exit.
sanitized() {
  local status=0 line
  "$@" 2> "$errors" || status=$?
  while IFS= read -r line; do printf '%s\n' "$(mask "$line")" >&2; done < "$errors"
  return "$status"
}

# The paths the path rules read: in the repository, every file git tracks, a file under an ignored
# path included, and every untracked file git does not ignore; under named paths, every file and
# directory find lists.
if [ ${#targets[@]} -eq 0 ]; then
  if ! sanitized git -c core.quotePath=false ls-files -z --cached --others --exclude-standard > "$matches"; then
    echo "check-private-names: git ls-files failed, so the repository's files are unknown" >&2
    exit 2
  fi
  sort -z -u "$matches" > "$paths"
else
  if ! sanitized find "${targets[@]}" -print0 > "$paths"; then
    echo "check-private-names: find failed over the named paths, so their files are unknown" >&2
    exit 2
  fi
fi
# The files the content rules read: the regular files among those paths, a symbolic link never
# followed, and no lockfile.
entries=0
regular=0
while IFS= read -r -d '' path; do
  entries=$((entries + 1))
  if [ -f "$path" ] && [ ! -L "$path" ]; then
    regular=$((regular + 1))
    for lockfile in "${lockfiles[@]}"; do
      if [ "${path##*/}" = "$lockfile" ]; then continue 2; fi
    done
    printf '%s\0' "$path"
  fi
done < "$paths" > "$files"
if [ ${#targets[@]} -eq 0 ]; then
  scanned="$entries files in the repository"
else
  shown=()
  for target in "${targets[@]}"; do shown+=("$(mask "$target")"); done
  scanned="$regular files under ${shown[*]}"
fi

# run <command...>: appends the command's output to $matches; a failed search (exit 2 or more,
# where grep's 1 means no match) stops the check.
run() {
  local status=0
  sanitized "$@" >> "$matches" || status=$?
  if [ "$status" -gt 1 ]; then
    echo "check-private-names: the search with $1 failed (exit $status)" >&2
    exit 2
  fi
}

# allowed <file> <match>: whether a committed rule's whole match names no private host: a reserved
# example host, a host under the reserved example tailnet, one of the two well-known names a
# pull request's text may hold, the NATS client's property in the bundle that vendors it, or one of
# the two Lean names, which are read in the match's own case.
allowed() {
  local match=${2,,} host="^($allowed_host)\$" tailnet_host="^($allowed_tailnet_host)\$"
  local bare_host="^($allowed_bare_host)\$"
  [[ $match =~ $host || $match =~ $tailnet_host ]] && return
  [[ $match =~ $bare_host ]] && return
  [[ $2 =~ ^$namespace_host || $2 =~ ^$lean_option$ ]] && return
  [[ $1 == "$vendored_nats" && $match =~ ^$vendored_property$ ]]
}

# search <label> <committed|term> <grep pattern option...>: appends one "line<TAB>label<TAB>path"
# record per line of a file where the pattern (`-E -e <rule>`, or `-E -i -f <patterns file>`)
# matches, searching every file of $files with one grep per batch xargs makes (an exit of 2 or
# more from any batch fails the search). Each batch runs under sh with no SHELLOPTS or BASHOPTS,
# so no option this script's caller exported (xtrace, errexit) reaches it. For a committed rule, a
# whole match that allowed names is dropped first; a term of the fifth rule has no such exception.
search() {
  local label=$1 kind=$2 file rest
  shift 2
  : > "$matches"
  # shellcheck disable=SC2016 # the batch's own shell expands "$@" and $?, not this one
  run xargs -0 -r env -u SHELLOPTS -u BASHOPTS sh -c \
    'grep "$@"; status=$?; [ "$status" -le 1 ] || exit 255' search -n -o -H -Z -I "$@" -- < "$files"
  while IFS= read -r -d '' file && IFS= read -r rest; do
    if [ "$kind" = committed ] && allowed "$file" "${rest#*:}"; then continue; fi
    printf '%s\t%s\t%s\0' "${rest%%:*}" "$label" "$file"
  done < "$matches" >> "$hits"
}

# search_paths <label> <grep pattern option...>: appends one "0<TAB>label in its path<TAB>path"
# record per file or directory whose path the pattern matches.
search_paths() {
  local label=$1 path
  shift
  # PRIVATE_NAME_CHECK_PATHS=no reads the text alone: check-pr-text.sh's run over the lines a
  # commit adds to a file it did not add or rename, whose path is no new text.
  [ "${PRIVATE_NAME_CHECK_PATHS:-yes}" = no ] && return 0
  : > "$matches"
  run grep -z "$@" "$paths"
  while IFS= read -r -d '' path; do
    printf '0\t%s in its path\t%s\0' "$label" "$path"
  done < "$matches" >> "$hits"
}

if [ "$rules" = names ]; then
  search "$repository_label" committed -E -i -e "$repository_rule"
  search "$company_label" committed -E -i -e "$company_rule"
  search "$company_label" committed -E -e "$proper_noun_rule"
  search "$private_host_label" committed -E -i -e "$private_host_rule"
  if [ "$bare_hosts" = yes ]; then
    search "$private_host_label" committed -E -i -e "$bare_host_rule"
  fi
  search "$tailnet_host_label" committed -E -i -e "$tailnet_host_rule"
  search_paths "$repository_label" -E -i -e "$repository_rule"
  search_paths "$company_label" -E -i -e "$company_rule"
  search_paths "$company_label" -E -e "$proper_noun_rule"
  if [ ${#terms[@]} -gt 0 ]; then
    search "$term_label" term -E -i -f "$patterns_file"
    search_paths "$term_label" -E -i -f "$patterns_file"
  fi
fi
# Rule 6: over everything a `keys` run reads. A key has no allowed form, so it is a `term` search,
# which drops nothing.
if [ "$rules" = keys ] && [ -n "$key_pattern" ]; then
  search "$issue_key_label" term -E -f "$key_file"
  search_paths "$issue_key_label" -E -f "$key_file"
fi

# escape_data <text>: text as a workflow command's message, its %, CR and LF escaped as GitHub
# reads them.
escape_data() {
  local text=${1//'%'/%25}
  text=${text//$'\r'/%0D}
  printf '%s' "${text//$'\n'/%0A}"
}

# escape_property <text>: text as a workflow command's property value: escaped as a message is,
# and its ":" and "," too.
escape_property() {
  local text
  text=$(escape_data "$1")
  text=${text//:/%3A}
  printf '%s' "${text//,/%2C}"
}

# The records name each file by its own path, so they are made unique by file and line before any
# path is masked: two files that print alike once masked count as two findings.
if [ -s "$hits" ]; then
  sort -z -t $'\t' -k3 -k1,1n -u -o "$hits" "$hits"
  findings=0
  while IFS= read -r -d '' hit; do
    findings=$((findings + 1))
    line=${hit%%$'\t'*}
    rest=${hit#*$'\t'}
    label=${rest%%$'\t'*}
    path=$(mask "${rest#*$'\t'}")
    if [ "$line" = 0 ]; then
      printf '::error::%s\n' "$(escape_data "$path: $label")"
    else
      printf '::error file=%s,line=%s::%s\n' "$(escape_property "$path")" "$line" \
        "$(escape_data "$path:$line: $label")"
    fi
  done < "$hits"
  echo "check-private-names: $findings finding(s) ($scanned). This repository is public, so:"
  echo "  - for the deployment repository write \"the deployment repository\" or <deployment repo>, and"
  echo "    use a neutral project key such as ACME in a fixture or example;"
  echo "  - for the company write \"the company\". Its first word capitalized or in capitals is the"
  echo "    company's name wherever it stands, at the start of a sentence or a heading or before a \"_\""
  echo "    too, so reword that sentence or rename that constant;"
  echo "  - for a host write a reserved example host such as <name>.internal.example. Code that only"
  echo "    looks like one, such as a field chain <value>.internal.<field> or a module file named"
  echo "    that way, is refused too: rename it, or reach the field through a variable;"
  echo "  - for a Tailscale MagicDNS host write <tailnet>.ts.net or <name>.<tailnet>.ts.net, or a"
  echo "    host under the reserved example-tailnet.ts.net. Code that only looks like one, such as"
  echo "    an unbraced shell variable glued right before the literal, is refused too: brace the"
  echo "    variable or build the suffix separately;"
  echo "  - for a term of PRIVATE_NAME_TERMS, the deployment's own list, write the neutral form above"
  echo "    that fits it (\"the deployment repository\", \"the company\", a placeholder id). The list"
  echo "    is an Actions secret, so this log never prints the term: read the reported line;"
  echo "  - for an issue key of the deployment's own projects write none, in a Dispatch link too: a"
  echo "    pull request's title, body, commits, comments and reviews carry no key but Legion's own"
  echo "    LEGION-<n>; the Dispatch issue links the pull request instead;"
  echo "  - for a file or directory whose path is reported with <name> in it, rename it."
  exit 1
fi

if [ "$rules" = keys ]; then
  if [ ${#keys[@]} -eq 0 ]; then
    echo "check-private-names: no issue key was checked: PRIVATE_NAME_TERMS is unset or empty" \
      "($scanned)"
  else
    echo "check-private-names: no text names an issue key of the deployment's own projects" \
      "(${#keys[@]} keys; $scanned)"
  fi
  exit 0
fi
named="the private deployment repository or the company"
if [ ${#terms[@]} -gt 0 ]; then
  named="the private deployment repository, the company or a term of PRIVATE_NAME_TERMS"
  named+=" (${#terms[@]} terms)"
fi
echo "check-private-names: no file or path names $named, and no text names a private internal" \
  "host or a Tailscale MagicDNS host ($scanned)"

#!/usr/bin/env bash
# Lists the committed tests that read, search or pin the words of a text an agent is given: a role
# prompt under packages/daemon/internal/prompts/ or the prompt the daemon composes from them, a
# skill or a task agent's definition, a text the daemon or a plugin sends an agent (a task, a
# notice, a reminder), and the description of a tool an agent calls.
#
# It is a search, not a gate. Each row is a line a reader then opens, to say whether the test it
# is in asserts words, which no pattern can decide: a test that composes the prompts to check the
# order of their parts reads them as structure, and one that checks a sentence is there reads
# words. So it errs on the side of listing, and a row is a place to read, not a finding.
#
# It reads the files git tracks in the work tree it is given, as they stand there: every Go test
# under any build tag (`*_test.go`: git reads a file whatever its build tags, `e2e` among them),
# the Bun tests (`*.test.ts`, `*.test.tsx`, `*.e2e.ts`), every file under a package's `test/` or
# `__tests__/` directory, and the shell harnesses and their libraries under `scripts/` (with
# `scripts/e2e/`), `.github/scripts/`, `.github/workflows/` and `packages/*/scripts/`. It leaves
# out its own two files, whose patterns and planted controls name exactly what it looks for.
#
# Each row carries the families its line matches:
#   prompt-package   a test of the prompts package itself, which reads the texts it embeds
#   prompt-file      a line naming a role prompt file or the prompts directory
#   composed-prompt  a line composing the role prompts, or reading an agent's system prompt
#   skill-file       a line naming a skill, a staged skill, a skill:// link or a task agent's file
#   tool-description a line reading a tool's description, its prompt lines, or the tool specs
#   task-text        a line calling a function of the daemon or a plugin whose name says it
#                    builds a text (`...Task`, `...Text`, `...Notice`, `...Sentence`, ...), the
#                    names read from the code at the time of the run, or reading a request's Task
#   asserted-words   an assertion, or a wanted value, holding a literal of four words or more, in
#                    a package whose code writes texts agents read, or in a file with a row above
#
# usage: scripts/prompt-word-tests.sh --repo <work tree> --out <dir> [--exclude <file>]
#   --repo <dir>      the work tree to search, e.g. a fresh clone of main
#   --out <dir>       where rows.tsv (file, line, families, the function or test the line is in,
#                     the text), files.txt (each file searched) and excluded.txt are written
#   --exclude <file>  repository paths another change owns, one a line, `#` starting a comment:
#                     their rows are left out and the paths listed in excluded.txt
#   --help            print this block and exit 0
#   exit: 0 nothing found, 1 rows found, 2 a usage error, 3 UNKNOWN: the tree could not be read,
#         or it held no file to search, so that "nothing found" would mean nothing
set -euo pipefail

name=prompt-word-tests

# Prints the header's usage block, from its `# usage:` line to the line before `set`.
usage() {
  sed -n '/^# usage: /,/^set -euo pipefail$/p' "$0" | sed '$d'
}

refuse() {
  echo "$name: $*" >&2
  echo "usage: scripts/prompt-word-tests.sh --repo <work tree> --out <dir> [--exclude <file>]" >&2
  exit 2
}

unknown() {
  echo "$name: UNKNOWN: $*"
  exit 3
}

repo="" out="" exclude=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) [ $# -ge 2 ] || refuse "--repo needs a directory"; repo=$2; shift 2 ;;
    --out) [ $# -ge 2 ] || refuse "--out needs a directory"; out=$2; shift 2 ;;
    --exclude) [ $# -ge 2 ] || refuse "--exclude needs a file"; exclude=$2; shift 2 ;;
    --help) usage; exit 0 ;;
    *) refuse "unknown argument: $1" ;;
  esac
done
[ -n "$repo" ] || refuse "--repo is required"
[ -n "$out" ] || refuse "--out is required"
if [ -n "$exclude" ] && [ ! -r "$exclude" ]; then
  refuse "cannot read the exclusion file $exclude"
fi

if ! top=$(git -C "$repo" rev-parse --show-toplevel 2> /dev/null) || [ -z "$top" ]; then
  unknown "$repo is not a git work tree this search can read"
fi
mkdir -p "$out"
out=$(cd "$out" && pwd)

# The files searched. A pathspec without magic matches across directories (`*_test.go` is every
# Go test at any depth); the glob ones name the directories a package keeps tests and helpers in.
pathspecs=(
  '*_test.go' '*.test.ts' '*.test.tsx' '*.e2e.ts'
  ':(glob)packages/*/test/**' ':(glob)packages/**/__tests__/**'
  'scripts/' '.github/scripts/' '.github/workflows/' ':(glob)packages/*/scripts/**'
  ':(exclude)scripts/prompt-word-tests.sh' ':(exclude)scripts/prompt-word-tests.test.sh'
)

: > "$out/excluded.txt"
if [ -n "$exclude" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    path=${line%%#*}
    path=$(printf '%s' "$path" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
    [ -n "$path" ] || continue
    if [ -z "$(git -C "$top" ls-files -- ":(literal)$path")" ]; then
      echo "$name: the exclusion names no tracked file: $path"
      continue
    fi
    printf '%s\n' "$path" >> "$out/excluded.txt"
    pathspecs+=(":(exclude,literal)$path")
  done < "$exclude"
fi

if ! git -C "$top" ls-files -- "${pathspecs[@]}" > "$out/files.txt"; then
  unknown "git could not list the tracked files of $top"
fi
searched=$(wc -l < "$out/files.txt" | tr -d ' ')
if [ "$searched" -eq 0 ]; then
  unknown "$top holds no test file to search"
fi

# The packages whose code writes texts that agents read: the daemon (prompts, tasks, notices), the
# plugins and the client they share (reminders, tool descriptions, rendered messages), the
# contracts (the Dispatch tools), and the live harnesses that drive agents.
agent_text_packages='^(packages/(daemon|pi-legion|pi-envoy|pi-shared|envoy-client|claude-envoy|contracts)/|scripts/e2e/)'

# The names of the functions that build a text for an agent, read from the code of those
# packages: a Go function whose result is a string or a notice, and a TypeScript function, whose
# name says it builds a text and is two words or more (a bare `Text` or `Message` is a method of
# many things).
text_suffix='(Task|Text|Notice|Sentence|Message|Prompt|Reminder|Words|Description)'
producers=$(
  {
    git -C "$top" grep -h -o -E "^func (\([^)]*\) )?[A-Za-z0-9_]+$text_suffix\([^)]*\) (string|record\.Notice|\(string, error\))" \
      -- 'packages/daemon/*.go' ':(exclude)*_test.go' 2> /dev/null |
      sed -E 's/^func (\([^)]*\) )?([A-Za-z0-9_]+)\(.*/\2/' || true
    git -C "$top" grep -h -o -E "^(export )?(async )?function [A-Za-z0-9_]+$text_suffix\(" \
      -- 'packages/pi-legion/*.ts' 'packages/pi-envoy/*.ts' 'packages/pi-shared/*.ts' 'packages/envoy-client/*.ts' \
      'packages/claude-envoy/*.ts' 'packages/contracts/*.ts' ':(exclude)*.test.ts' 2> /dev/null |
      sed -E 's/^(export )?(async )?function ([A-Za-z0-9_]+)\(/\3/' || true
  } | { grep -E '^[a-z]+[A-Z]|^[A-Z][a-z0-9]+[A-Z]' || true; } | sort -u | paste -sd '|' -
)

q="'"
bt='`'
re_prompt_file="internal/prompts([/\"$q$bt]|\$)|prompts/(go|roles)/|roles/(core|mechanics)/|(worker|architect)-common\\.md|architect-root\\.md|controller-(root|headless|interactive)\\.md|claude-mechanics\\.md"
re_composed="prompts\\.(New|Compose|Part|RoleReferences)\\(|[Cc]omposer?\\.(Compose|Part)\\(|(^|[^A-Za-z0-9_.])(Compose|composeRole|rolePrompt|RolePrompt|composedPrompt|ComposedPrompt)\\(|[Ss]ystemPrompt|append-system-prompt"
re_skill="(^|[^A-Za-z0-9_.-])skills/|SKILL\\.md|skill://|stage-skills|pi-legion/agents|(^|[^A-Za-z0-9_])agents/[a-z0-9-]+\\.md|[Ss]kills?(Root|Dir|Path|Files?)([^A-Za-z0-9_]|\$)|[Aa]gents(Root|Dir)([^A-Za-z0-9_]|\$)|references/[a-z0-9-]+\\.md"
re_tool="promptSnippet|promptGuidelines|[A-Za-z]*ToolSpecs|toolSpecs?([^A-Za-z0-9_]|\$)|[Tt]ool[A-Za-z]*\\??\\.(description|Description)|(^|[^A-Za-z0-9_])(spec|tool|definition|schema|parameter|property|field)s?\\??(\\[[^]]*\\])?\\??\\.description|\\.properties\\.[A-Za-z_]+\\??\\.description|inputSchema|tool-contract"
re_task="\\.Task([^A-Za-z0-9_]|\$)"
if [ -n "$producers" ]; then
  re_task="(^|[^A-Za-z0-9_])($producers)\\(|$re_task"
fi
# An assertion of a text, or a value a test wants, and a literal of four words or more. A list of
# wanted phrases, one a line, is not an assertion line, but its test has a row of another family
# (the text it reads), and the reader reads the whole test.
re_assert="strings\\.(Contains|ContainsAny|Count|Index|HasPrefix|HasSuffix|EqualFold|CutPrefix|TrimPrefix)\\(|bytes\\.(Contains|HasPrefix|HasSuffix)\\(|regexp\\.(MustCompile|MatchString)\\(|slices\\.Contains\\(|\\.(toContain|toMatch|toInclude|toStartWith|toEndWith|toBe|toEqual|toStrictEqual|includes|startsWith|endsWith)\\(|new RegExp\\(|(^|[^A-Za-z0-9_-])grep( |\$)|(^|[^A-Za-z0-9_])(want|wanted|says|said|never|gone|phrases?|sentences?|expected)[A-Za-z0-9_]* *:?= *"
re_prose="[\"$q$bt/][^\"$q$bt]*[A-Za-z]+ +[A-Za-z$q(),.:;-]+ +[A-Za-z$q(),.:;-]+ +[A-Za-z]"

raw="$out/raw.txt"
: > "$raw"
# search <family> <grep arguments...>: appends `file<TAB>line<TAB>family<TAB>text` for each line.
search() {
  local family=$1
  shift
  local status=0
  git -C "$top" grep -n -I -E "$@" > "$out/grep.txt" 2> "$out/grep.err" || status=$?
  if [ "$status" -gt 1 ]; then
    unknown "git grep failed on the $family family: $(head -n 3 "$out/grep.err")"
  fi
  awk -v family="$family" '{
    first = index($0, ":"); file = substr($0, 1, first - 1); rest = substr($0, first + 1)
    second = index(rest, ":"); line = substr(rest, 1, second - 1); text = substr(rest, second + 1)
    gsub(/\t/, " ", text)
    print file "\t" line "\t" family "\t" text
  }' "$out/grep.txt" >> "$raw"
}

search prompt-package -e '^func (Test|Fuzz|Benchmark|Example)' -- 'packages/daemon/internal/prompts/*_test.go' "${pathspecs[@]:10}"
search prompt-file -e "$re_prompt_file" -- "${pathspecs[@]}"
search composed-prompt -e "$re_composed" -- "${pathspecs[@]}"
search skill-file -e "$re_skill" -- "${pathspecs[@]}"
search tool-description -e "$re_tool" -- "${pathspecs[@]}"
search task-text -e "$re_task" -- "${pathspecs[@]}"
cut -f1 "$raw" | sort -u > "$out/source-files.txt"
search asserted-words -e "$re_assert" --and -e "$re_prose" -- "${pathspecs[@]}"

# Keep an asserted-words row where the code of its package writes texts agents read, or where its
# file has a row of another family; one row a line, its families joined.
awk -F'\t' -v packages="$agent_text_packages" '
  NR == FNR { source[$0] = 1; next }
  $3 == "asserted-words" && !($1 ~ packages) && !($1 in source) { next }
  {
    key = $1 "\t" $2
    if (!(key in families)) { order[++count] = key; text[key] = $4; families[key] = $3 }
    else if (index("," families[key] ",", "," $3 ",") == 0) { families[key] = families[key] "," $3 }
  }
  END { for (i = 1; i <= count; i++) { k = order[i]; print k "\t" families[k] "\t" text[k] } }
' "$out/source-files.txt" "$raw" | sort -t$'\t' -k1,1 -k2,2n > "$out/rows.untitled.tsv"

# The function or test each row is in: the last Go `func`, Bun `test`/`it`/`describe`, or shell
# `begin` or function header above its line.
# The files are read in one pass, from the tree's root, so each is named as rows.tsv names it.
cut -f1 "$out/rows.untitled.tsv" | uniq > "$out/row-files.txt"
: > "$out/rows.tsv"
if [ -s "$out/row-files.txt" ]; then
  (
    cd "$top"
    tr '\n' '\0' < "$out/row-files.txt" | xargs -0 awk -F'\t' -v OFS='\t' -v rows="$out/rows.untitled.tsv" '
      BEGIN { while ((getline row < rows) > 0) { split(row, parts, "\t"); want[parts[1] "\t" parts[2]] = row } }
      FNR == 1 { current = "" }
      {
        if (match($0, /^func (\([^)]*\) )?[A-Za-z0-9_]+/)) {
          header = substr($0, RSTART, RLENGTH); sub(/^func (\([^)]*\) )?/, "", header); current = header
        } else if (match($0, /^[[:space:]]*(test|it|describe)(\.[a-z]+(\([^)]*\))?)?\([[:space:]]*["\047`]/)) {
          title = substr($0, RSTART + RLENGTH); quote = substr($0, RSTART + RLENGTH - 1, 1)
          end = index(title, quote); if (end > 0) title = substr(title, 1, end - 1)
          current = substr(title, 1, 120)
        } else if (match($0, /^begin [A-Za-z0-9_.-]+/)) {
          current = substr($0, 7, RLENGTH - 6)
        } else if (match($0, /^[A-Za-z_][A-Za-z0-9_]*\(\) *\{/)) {
          current = substr($0, 1, index($0, "(") - 1)
        }
        key = FILENAME "\t" FNR
        if (key in want) {
          split(want[key], parts, "\t")
          print parts[1], parts[2], parts[3], current, substr(parts[4], 1, 400)
        }
      }
    ' > "$out/rows.titled.tsv"
  )
  sort -t$'\t' -k1,1 -k2,2n "$out/rows.titled.tsv" > "$out/rows.tsv"
fi
rm -f "$out/rows.untitled.tsv" "$out/rows.titled.tsv" "$out/row-files.txt" "$out/grep.txt" "$out/grep.err" "$out/raw.txt" "$out/source-files.txt"

rows=$(wc -l < "$out/rows.tsv" | tr -d ' ')
files=$(cut -f1 "$out/rows.tsv" | sort -u | wc -l | tr -d ' ')
awk -F'\t' '{ text = $5; sub(/^[[:space:]]+/, "", text); print $1 ":" $2 ": " $3 ": " substr(text, 1, 200) }' "$out/rows.tsv"
while IFS= read -r path; do
  echo "$name: excluded: $path"
done < "$out/excluded.txt"
echo "$name: $rows row(s) in $files file(s), of $searched test file(s) searched"
if [ "$rows" -gt 0 ]; then
  exit 1
fi

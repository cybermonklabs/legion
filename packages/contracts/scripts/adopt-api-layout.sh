#!/usr/bin/env bash
# Resolves the Dispatch API's generated files in a merge of main into a branch made before main
# changed how they are kept. Run it from anywhere in the checkout once the merge holds conflicts,
# or after a merge that did not stop:
#
#   packages/contracts/scripts/adopt-api-layout.sh
#
# with git, after `git merge origin/main` (or a rebase step) stops; with jj, with the merge as the
# working copy, after `jj new <branch> main@origin` (for a rebased stack, with the conflicted
# commit as @). A checkout `jj root` answers for is resolved with jj, since jj tracks files itself
# and reads no git index; every other checkout with git.
#
# It changes only these generated files. Each other conflict stays yours: it lists them when it
# ends. Run it again after you resolve a Go file it names.
#
#   1. It refuses to run before main's root .gitignore names
#      packages/contracts/src/dispatch-api.generated.ts, where it would only delete the file from
#      the branch. It reads that line itself: a local or global exclude is not main's layout.
#   2. packages/contracts/schemas/dispatch-openapi.json is emitted from the Go route table, so a
#      conflict in it is resolved by emitting it again from the merged Go
#      (`go run ./cmd/dispatch openapi --out ...`), never by hand. That waits until no Go file of
#      packages/envoy is still in conflict, since the document is emitted from them.
#   3. The generated types are rendered again from the document the tree now holds.
#   4. The module leaves the commit (`git rm --cached`, `jj file untrack`) and stays on disk,
#      ignored: main holds it in no commit and every `bun install` renders it. A branch that changed
#      it met main's deletion as a conflict, which this resolves.
#   5. It fails while the commit still holds the module, so a merge that would carry it back stops
#      here rather than reaching main.
#
# A step that main's later layout changes add (the document's line format, one file per route)
# is added here by the change that makes it.
set -euo pipefail

generated=packages/contracts/src/dispatch-api.generated.ts
document=packages/contracts/schemas/dispatch-openapi.json

if root=$(jj root 2>/dev/null); then
  vcs=jj
else
  root=$(git rev-parse --show-toplevel)
  vcs=git
fi
cd "$root"

# The conflicted paths under the paths given (all of them with none), one per line.
unmerged() {
  if [ "$vcs" = jj ]; then
    jj file list -r @ -T 'if(conflict, path ++ "\n")' -- "$@"
  else
    git diff --name-only --diff-filter=U -- "$@"
  fi
}

# The module, if the commit being made still holds it.
tracked() {
  if [ "$vcs" = jj ]; then
    jj file list -r @ -- "$generated" 2>/dev/null
  else
    git ls-files -- "$generated"
  fi
}

# Step 1.
if ! grep -qxF -- "$generated" .gitignore; then
  echo "adopt-api-layout: the root .gitignore does not name $generated, so main's layout is not in this" \
    "tree yet: merge main first, then run this again" >&2
  exit 1
fi

# Step 2.
if [ -n "$(unmerged "$document")" ]; then
  go_conflicts=$(unmerged packages/envoy)
  if [ -n "$go_conflicts" ]; then
    echo "adopt-api-layout: $document is emitted from the Go route table, and these files of it are" \
      "still in conflict; resolve them, then run this again:" >&2
    printf '  %s\n' "${go_conflicts//$'\n'/$'\n'  }" >&2
    exit 1
  fi
  (cd packages/envoy && go run ./cmd/dispatch openapi --out "../../$document")
  if [ "$vcs" = git ]; then
    git add -- "$document"
  fi
  echo "adopt-api-layout: $document emitted again from the merged route table"
fi

# Step 3.
if [ -n "$(unmerged "$document")" ] || ! bun packages/contracts/scripts/gen-api.ts; then
  echo "adopt-api-layout: $generated was not rendered: run \`bun install\` at the repository root" \
    "once the document has no conflict, then run this again" >&2
  exit 1
fi
echo "adopt-api-layout: $generated rendered from $document"

# Step 4. A refusal here (jj refuses to untrack a file no ignore rule covers) is step 5's to report.
if [ -n "$(tracked)" ]; then
  if [ "$vcs" = jj ]; then
    jj file untrack -- "$generated" || true
  else
    git rm -q --cached -- "$generated" || true
  fi
fi

# Step 5.
if [ -n "$(tracked)" ]; then
  echo "adopt-api-layout: the commit still holds $generated, which main holds in no commit: take it" \
    "out (\`git rm --cached\`, or \`jj file untrack\` once no ignore rule re-includes it) before this" \
    "merge goes further" >&2
  exit 1
fi
echo "adopt-api-layout: $generated is in no commit (every bun install renders it); the file stays on disk"

left=$(unmerged)
if [ -n "$left" ]; then
  echo "adopt-api-layout: still in conflict, yours to resolve:"
  printf '  %s\n' "${left//$'\n'/$'\n'  }"
fi

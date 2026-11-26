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
# It changes only these generated files: the two OpenAPI documents, one per version of the API
# (packages/contracts/schemas/dispatch-openapi.json for /api/v1, dispatch-openapi-v2.json for
# /api/v2), and the module scripts/gen-api.ts renders from each
# (packages/contracts/src/dispatch-api.generated.ts, dispatch-api-v2.generated.ts). Each other
# conflict stays yours: it lists them when it ends. Run it again after you resolve a Go file it
# names.
#
#   1. It refuses to run before main's root .gitignore names
#      packages/contracts/src/dispatch-api.generated.ts, where it would only delete the modules from
#      the branch. It reads that line itself: a local or global exclude is not main's layout.
#   2. The documents are emitted from the Go route table, so a conflict in either is resolved by
#      emitting both again from the merged Go (`go run ./cmd/dispatch openapi`, which writes each
#      where the repository commits it), never by hand. That waits until no Go file of
#      packages/envoy is still in conflict, since the documents are emitted from them.
#   3. The generated types are rendered again from the documents the tree now holds.
#   4. Each module leaves the commit (`git rm --cached`, `jj file untrack`) and stays on disk,
#      ignored: main holds neither in a commit and every `bun install` renders both. A branch that
#      changed one met main's deletion as a conflict, which this resolves.
#   5. It fails while the commit still holds a module, so a merge that would carry one back stops
#      here rather than reaching main.
#
# A step that main's later layout changes add (the document's line format, one file per route)
# is added here by the change that makes it.
set -euo pipefail

generated=(packages/contracts/src/dispatch-api.generated.ts packages/contracts/src/dispatch-api-v2.generated.ts)
documents=(packages/contracts/schemas/dispatch-openapi.json packages/contracts/schemas/dispatch-openapi-v2.json)

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

# The modules the commit being made still holds, one per line.
tracked() {
  if [ "$vcs" = jj ]; then
    jj file list -r @ -- "${generated[@]}" 2>/dev/null
  else
    git ls-files -- "${generated[@]}"
  fi
}

# Step 1.
if ! grep -qxF -- "${generated[0]}" .gitignore; then
  echo "adopt-api-layout: the root .gitignore does not name ${generated[0]}, so main's layout is not in" \
    "this tree yet: merge main first, then run this again" >&2
  exit 1
fi

# Step 2.
if [ -n "$(unmerged "${documents[@]}")" ]; then
  go_conflicts=$(unmerged packages/envoy)
  if [ -n "$go_conflicts" ]; then
    echo "adopt-api-layout: ${documents[*]} are emitted from the Go route table, and these files of it" \
      "are still in conflict; resolve them, then run this again:" >&2
    printf '  %s\n' "${go_conflicts//$'\n'/$'\n'  }" >&2
    exit 1
  fi
  (cd packages/envoy && go run ./cmd/dispatch openapi)
  if [ "$vcs" = git ]; then
    git add -- "${documents[@]}"
  fi
  echo "adopt-api-layout: ${documents[*]} emitted again from the merged route table"
fi

# Step 3.
if [ -n "$(unmerged "${documents[@]}")" ] || ! bun packages/contracts/scripts/gen-api.ts; then
  echo "adopt-api-layout: ${generated[*]} were not rendered: run \`bun install\` at the repository" \
    "root once the documents have no conflict, then run this again" >&2
  exit 1
fi
echo "adopt-api-layout: ${generated[*]} rendered from ${documents[*]}"

# Step 4. A refusal here (jj refuses to untrack a file no ignore rule covers) is step 5's to report.
while IFS= read -r module; do
  [ -n "$module" ] || continue
  if [ "$vcs" = jj ]; then
    jj file untrack -- "$module" || true
  else
    git rm -q --cached -- "$module" || true
  fi
done <<<"$(tracked)"

# Step 5.
held=$(tracked || true)
if [ -n "$held" ]; then
  echo "adopt-api-layout: the commit still holds ${held//$'\n'/ and }, which main holds in no commit:" \
    "take each out (\`git rm --cached\`, or \`jj file untrack\` once no ignore rule re-includes it)" \
    "before this merge goes further" >&2
  exit 1
fi
echo "adopt-api-layout: ${generated[*]} are in no commit (every bun install renders them); the files" \
  "stay on disk"

left=$(unmerged)
if [ -n "$left" ]; then
  echo "adopt-api-layout: still in conflict, yours to resolve:"
  printf '  %s\n' "${left//$'\n'/$'\n'  }"
fi

# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-versions.sh
#
# The --clients scenarios of the developer box's versions. daemon-release-gate.test.sh sources this
# file after the client fixtures (daemon-release-gate-fixtures-clients.sh), whose box_case it uses;
# it is not run on its own.
#
# A version is judged by the commit of its release tag, so it must be one exact release version, as
# Semantic Versioning writes one. The claude-envoy version the marketplace serves is the one npm's
# own pick takes on this box (`npm pack --dry-run --json`), so the gate judges that version alone,
# and an npm answer that is not one package named @sjawhar/claude-envoy is unreadable whole. A
# marketplace that names claude-envoy twice is read as neither.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

readonly npm_answer="_sjawhar_claude-envoy___1.0.0"

echo "case: --clients, npm takes a version behind the commit"
box_case versions-npm-behind
printf '3.3.0\n' >"${case_root}/npm/${npm_answer}"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming the version npm takes" \
  "$(has_line "GATE SHUT: dotfiles .claude/settings.json runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12)${claude_note}")"
check "judges no other version" "$(is "$(grep -c '^[A-Z ]*: dotfiles .claude/settings.json runs claude-envoy ' "$out")" 1)"

echo "case: --clients, npm takes a pre-release a release tag names"
box_case versions-npm-pre-release
printf '4.1.0-rc.1\n' >"${case_root}/npm/${npm_answer}"
tag claude-envoy-v4.1.0-rc.1 "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads it as the exact version it is" \
  "$(has_line "PASS: dotfiles .claude/settings.json runs claude-envoy 4.1.0-rc.1 from claude-envoy-v4.1.0-rc.1 at ${ahead_rev} (ahead of ${commit})${claude_note}")"

echo "case: --clients, npm takes a version with build metadata"
box_case versions-npm-build
printf '4.0.0+build.1\n' >"${case_root}/npm/${npm_answer}"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, the version kept, which no release tag carries" \
  "$(has_kept_line "UNKNOWN: dotfiles .claude/settings.json: claude-envoy names a version that is no exact release version, kept in KEPT")"
check "the kept file holds the version" "$(is "$(kept_text "UNKNOWN: dotfiles .claude/settings.json: ")" "4.0.0+build.1")"

# Each answer below is npm's whole answer, printed as it is (`.raw`): none is the one package named
# @sjawhar/claude-envoy that `npm pack --dry-run --json` answers, so the gate reads it as no answer.
for answer in '[]' '{"name": "@sjawhar/claude-envoy", "version": "4.0.0"}' \
  '[{"name": "@sjawhar/claude-envoy", "version": "4.0.0"}, {"name": "@sjawhar/claude-envoy", "version": "3.3.0"}]' \
  '[{"name": "@other/claude-envoy", "version": "4.0.0"}]' '[{"name": "@sjawhar/claude-envoy", "version": 4}]' \
  '[{"name": "@sjawhar/claude-envoy"}]' '["4.0.0"]' 'text'; do
  echo "case: --clients, npm answers ${answer}"
  box_case "versions-npm-shape-$(printf '%s' "$answer" | tr -c 'A-Za-z0-9' '_')"
  if [ "$answer" = text ]; then
    printf 'npm warn config something\n4.0.0\n' >"${case_root}/npm/${npm_answer}.raw"
  else
    printf '%s\n' "$answer" >"${case_root}/npm/${npm_answer}.raw"
  fi
  run_gate -- --clients "$commit"
  check "exits 3" "$(is "$status" 3)"
  check "refuses UNKNOWN" \
    "$(has_line "UNKNOWN: dotfiles .claude/settings.json: npm answers no version of @sjawhar/claude-envoy for >=1.0.0")"
  check "the verdict is the last line" "$(last_line_is "UNKNOWN")"
done

echo "case: --clients, npm cannot be reached"
box_case versions-npm-down
run_gate FAKE_NPM_DOWN=1 -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, npm's words kept" \
  "$(has_kept_line "UNKNOWN: dotfiles .claude/settings.json: npm cannot resolve @sjawhar/claude-envoy@>=1.0.0: npm failed; its own words are kept in KEPT")"
check "the kept file holds npm's words" \
  "$(grep -qF "npm error code ENOTFOUND" <<<"$(kept_text "npm cannot resolve")" && echo true || echo false)"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a marketplace with two claude-envoy entries"
box_case versions-marketplace-twice
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": ">=1.0.0"}}, {"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": "1.0.0"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, choosing neither, the marketplace kept" \
  "$(has_kept_line "UNKNOWN: dotfiles .claude/settings.json: example-org/plugins .claude-plugin/marketplace.json serves claude-envoy as no one entry of the npm package @sjawhar/claude-envoy; the marketplace is kept in KEPT")"
check "asks npm nothing" "$(lacks "npm " "$calls")"

echo "case: --clients, a marketplace that serves claude-envoy from no npm package"
box_case versions-marketplace-not-npm
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "github", "repo": "example-org/claude-envoy"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, the marketplace kept, not its source printed" \
  "$(has_kept_line "UNKNOWN: dotfiles .claude/settings.json: example-org/plugins .claude-plugin/marketplace.json serves claude-envoy as no one entry of the npm package @sjawhar/claude-envoy; the marketplace is kept in KEPT")"
check "the kept marketplace holds the source it served" \
  "$(grep -qF 'example-org/claude-envoy' <<<"$(kept_text "UNKNOWN: dotfiles .claude/settings.json: ")" && echo true || echo false)"

echo "case: --clients, a developer-box pin that is no exact version"
box_case versions-pin-range
dotfiles "^17.40.0"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, the pin kept" \
  "$(has_kept_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy names a version that is no exact release version, kept in KEPT")"
check "the kept file holds the pin" "$(is "$(kept_text "UNKNOWN: dotfiles omp/plugins/package.json: ")" "^17.40.0")"

echo "case: --clients, a pin with a leading zero in its pre-release"
box_case versions-pin-leading-zero
dotfiles "17.41.0-01"
tag "pi-envoy-v17.41.0-01" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, the pin kept" \
  "$(has_kept_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy names a version that is no exact release version, kept in KEPT")"

echo "case: --clients, a pre-release pin a release tag names"
box_case versions-pre-release
dotfiles "17.41.0-rc.1"
tag "pi-envoy-v17.41.0-rc.1" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "reads it as the exact version it is" \
  "$(has_line "PASS: dotfiles omp/plugins/package.json runs pi-envoy 17.41.0-rc.1 from pi-envoy-v17.41.0-rc.1 at ${ahead_rev} (ahead of ${commit})")"

echo "case: --clients, a pin no release tag names"
box_case versions-no-tag
dotfiles "17.99.0"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the tag, gh's words kept" \
  "$(has_kept_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy 17.99.0 has no release tag pi-envoy-v17.99.0 in cybermonklabs/legion: gh failed (HTTP 422); its own words are kept in KEPT")"
check "the kept file holds gh's words" \
  "$(is "$(kept_text "UNKNOWN: dotfiles omp/plugins/package.json: ")" "gh: No commit found for SHA: refs/tags/pi-envoy-v17.99.0 (HTTP 422)")"

echo "case: --clients, a release tag GitHub answers with no commit id"
box_case versions-tag-not-commit
tag pi-envoy-v17.40.0 "main"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, the answer kept, never comparing it" \
  "$([ "$(has_kept_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy 17.40.0 has no release tag pi-envoy-v17.40.0 in cybermonklabs/legion: GitHub answered something that is no commit id, kept in KEPT")" = true ] && lacks "...main" "$calls" || echo false)"

# What the marketplace and the dotfiles name is their text, not the gate's: it is printed with each
# control character written as \x<hh>, so neither can start a line of the gate's output.
echo "case: --clients, a marketplace range that holds a newline and a verdict"
box_case versions-range-newline
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": "x\\nGATE OPEN"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "keeps the range, its newline and all, and prints none of it" \
  "$(has_kept_line "UNKNOWN: dotfiles .claude/settings.json: npm cannot resolve @sjawhar/claude-envoy@<the version kept in KEPT>: npm failed; its own words are kept in KEPT")"
check "no line of its output is the range's verdict" "$(grep -q '^GATE OPEN' "$out" && echo false || echo true)"

echo "case: --clients, a dotfiles pin that holds a newline and a verdict"
box_case versions-pin-newline
dotfiles '17.40.0\nGATE OPEN'
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "keeps the pin, its newline and all, and prints none of it" \
  "$(has_kept_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy names a version that is no exact release version, kept in KEPT")"
check "no line of its output is the pin's verdict" "$(grep -q '^GATE OPEN' "$out" && echo false || echo true)"

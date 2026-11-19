# shellcheck shell=bash
# packages/envoy/scripts/lib/daemon-release-gate-cases-versions.sh
#
# The --clients scenarios of the developer box's versions. daemon-release-gate.test.sh sources this
# file after the client fixtures (daemon-release-gate-fixtures-clients.sh), whose box_case it uses;
# it is not run on its own.
#
# A version is judged by the commit of its release tag, so it must be one exact release version, as
# Semantic Versioning writes one. npm's install takes the package's `latest` dist-tag when it
# matches the marketplace's range, else the highest version that matches, so the gate judges both:
# `latest` when it is among the versions npm answers for the range, and the highest of them by
# semantic-version precedence, where a release ranks above its own pre-release and numbers compare
# as numbers. An npm answer holding anything that is no such version is unreadable whole, and a
# marketplace that names claude-envoy twice is read as neither.
# shellcheck disable=SC2154 # every variable read here is set by the harness or the test that sources this file

readonly npm_answer="_sjawhar_claude-envoy___1.0.0"

echo "case: --clients, npm matches a release and its own pre-release"
box_case versions-npm-pre-release
printf '["4.0.0-rc.1", "4.0.0"]\n' >"${case_root}/npm/${npm_answer}"
tag claude-envoy-v4.0.0 "$behind_rev"
tag claude-envoy-v4.0.0-rc.1 "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "judges the release npm serves, not its pre-release" \
  "$(has_line "GATE SHUT: dotfiles .claude/settings.json runs claude-envoy 4.0.0 from claude-envoy-v4.0.0 at ${behind_rev} (behind ${commit} by 12)${claude_note}")"

echo "case: --clients, npm matches versions whose numbers sort apart as text"
box_case versions-npm-numeric
printf '["4.10.0", "4.9.0"]\n' >"${case_root}/npm/${npm_answer}"
tag claude-envoy-v4.10.0 "$ahead_rev"
tag claude-envoy-v4.9.0 "$behind_rev"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "judges 4.10.0, the highest by precedence" \
  "$(has_line "PASS: dotfiles .claude/settings.json runs claude-envoy 4.10.0 from claude-envoy-v4.10.0 at ${ahead_rev} (ahead of ${commit})${claude_note}")"

echo "case: --clients, npm matches one version, which it answers as a string"
box_case versions-npm-one
printf '"4.0.0"\n' >"${case_root}/npm/${npm_answer}"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "judges that version" "$(has_line "PASS: dotfiles .claude/settings.json runs ${claude_served}")"

echo "case: --clients, npm's latest dist-tag below the highest match and behind the commit"
box_case versions-npm-latest-lower
printf '{"latest": "3.3.0"}\n' >"${case_root}/npm/_sjawhar_claude-envoy.dist-tags"
run_gate -- --clients "$commit"
check "exits 1" "$(is "$status" 1)"
check "shuts naming latest, the version npm installs" \
  "$(has_line "GATE SHUT: dotfiles .claude/settings.json runs claude-envoy 3.3.0 from claude-envoy-v3.3.0 at ${behind_rev} (behind ${commit} by 12); the version legion-plugins serves for >=1.0.0, npm's latest dist-tag")"
check "still judges the highest match on its own line" \
  "$(has_line "PASS: dotfiles .claude/settings.json runs claude-envoy 4.0.0 from claude-envoy-v4.0.0 at ${identical_rev} (identical to ${commit}); the highest version npm matches to >=1.0.0")"
check "the verdict is GATE SHUT" "$(last_line_is "GATE SHUT")"

echo "case: --clients, npm's latest dist-tag outside the marketplace's range"
box_case versions-npm-latest-outside
printf '{"latest": "0.9.0"}\n' >"${case_root}/npm/_sjawhar_claude-envoy.dist-tags"
tag claude-envoy-v0.9.0 "$behind_rev"
run_gate -- --clients "$commit"
check "exits 0" "$(is "$status" 0)"
check "asks npm for the latest dist-tag" \
  "$(grep -qxF "npm view @sjawhar/claude-envoy dist-tags --json" "$calls" && echo true || echo false)"
check "judges the highest match alone, as the version served" "$(has_line "PASS: dotfiles .claude/settings.json runs ${claude_served}")"
check "never judges latest" "$([ "$(lacks "claude-envoy-v0.9.0" "$calls")" = true ] && lacks "0.9.0" "$out" || echo false)"

echo "case: --clients, npm answers no latest dist-tag"
box_case versions-npm-no-latest
printf '{"next": "4.0.0"}\n' >"${case_root}/npm/_sjawhar_claude-envoy.dist-tags"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the dist-tag" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: npm answers no latest dist-tag of @sjawhar/claude-envoy")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

for answer in '["4.0.0-01"]' '["4.0.0-rc..1"]' '["4.0.0-."]' '["3.0.0-01", "4.0.0"]' '["3.0.0+..", "4.0.0"]' '["1.0.0\n4.0.0"]' '["4.0.0", {"version": "1.0.0"}]' 'text'; do
  echo "case: --clients, npm answers ${answer}"
  box_case "versions-npm-grammar-$(printf '%s' "$answer" | tr -c 'A-Za-z0-9' '_')"
  if [ "$answer" = text ]; then
    printf 'npm warn config something\n4.0.0\n' >"${case_root}/npm/${npm_answer}"
  else
    printf '%s\n' "$answer" >"${case_root}/npm/${npm_answer}"
  fi
  tag "claude-envoy-v4.0.0-01" "$ahead_rev"
  tag "claude-envoy-v4.0.0-rc..1" "$ahead_rev"
  tag "claude-envoy-v4.0.0-." "$ahead_rev"
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
check "refuses UNKNOWN naming npm's answer" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: npm cannot resolve @sjawhar/claude-envoy@>=1.0.0: npm error code ENOTFOUND")"
check "the verdict is UNKNOWN, never GATE OPEN" "$(last_line_is "UNKNOWN")"

echo "case: --clients, a marketplace with two claude-envoy entries"
box_case versions-marketplace-twice
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": ">=1.0.0"}}, {"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": "1.0.0"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN, choosing neither" \
  "$(grep -q '^UNKNOWN: dotfiles .claude/settings.json: example-org/plugins .claude-plugin/marketplace.json serves claude-envoy as .*2 entries name claude-envoy' "$out" && echo true || echo false)"
check "asks npm nothing" "$(lacks "npm view" "$calls")"

echo "case: --clients, a marketplace that serves claude-envoy from no npm package"
box_case versions-marketplace-not-npm
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "github", "repo": "example-org/claude-envoy"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the source" \
  "$(has_line 'UNKNOWN: dotfiles .claude/settings.json: example-org/plugins .claude-plugin/marketplace.json serves claude-envoy as {"source":"github","repo":"example-org/claude-envoy"}, not as one entry of the npm package @sjawhar/claude-envoy')"

echo "case: --clients, a developer-box pin that is no exact version"
box_case versions-pin-range
dotfiles "^17.40.0"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy ^17.40.0 is no exact release version")"

echo "case: --clients, a pin with a leading zero in its pre-release"
box_case versions-pin-leading-zero
dotfiles "17.41.0-01"
tag "pi-envoy-v17.41.0-01" "$ahead_rev"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the pin" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy 17.41.0-01 is no exact release version")"

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
check "refuses UNKNOWN naming the tag and GitHub's answer" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy 17.99.0 has no release tag pi-envoy-v17.99.0 in cybermonklabs/legion: gh: No commit found for SHA: refs/tags/pi-envoy-v17.99.0 (HTTP 422)")"

echo "case: --clients, a release tag GitHub answers with no commit id"
box_case versions-tag-not-commit
tag pi-envoy-v17.40.0 "main"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "refuses UNKNOWN naming the answer, never comparing it" \
  "$([ "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy 17.40.0 has no release tag pi-envoy-v17.40.0 in cybermonklabs/legion: GitHub answered main, which is no commit id")" = true ] && lacks "...main" "$calls" || echo false)"

# What the marketplace and the dotfiles name is their text, not the gate's: it is printed with each
# control character written as \x<hh>, so neither can start a line of the gate's output.
echo "case: --clients, a marketplace range that holds a newline and a verdict"
box_case versions-range-newline
printf '{"name": "legion-plugins", "plugins": [{"name": "claude-envoy", "source": {"source": "npm", "package": "@sjawhar/claude-envoy", "version": "x\\nGATE OPEN"}}]}\n' \
  >"${case_root}/marketplaces/example-org/plugins.json"
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "names the range, its newline written out" \
  "$(has_line "UNKNOWN: dotfiles .claude/settings.json: npm cannot resolve @sjawhar/claude-envoy@x\\x0aGATE OPEN: npm error code E404")"
check "no line of its output is the range's verdict" "$(grep -q '^GATE OPEN' "$out" && echo false || echo true)"

echo "case: --clients, a dotfiles pin that holds a newline and a verdict"
box_case versions-pin-newline
dotfiles '17.40.0\nGATE OPEN'
run_gate -- --clients "$commit"
check "exits 3" "$(is "$status" 3)"
check "names the pin, its newline written out" \
  "$(has_line "UNKNOWN: dotfiles omp/plugins/package.json: pi-envoy 17.40.0\\x0aGATE OPEN is no exact release version")"
check "no line of its output is the pin's verdict" "$(grep -q '^GATE OPEN' "$out" && echo false || echo true)"

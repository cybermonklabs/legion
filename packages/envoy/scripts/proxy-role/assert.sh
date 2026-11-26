# shellcheck shell=bash
# packages/envoy/scripts/proxy-role/assert.sh: the placeholder assertion of proxy-role-check.sh,
# which its test (proxy-role-check.test.sh) drives on its own.

# assert_placeholders_only <bundle root>: every hosts.yml under the root holds broker placeholders
# alone, at least one hosts.yml is read, and no file under the root holds a GitHub token. It prints
# what it read, by count and kind and never a value, and returns 0 only when all of that holds. A
# root with no hosts.yml under it is UNKNOWN, which fails: an assertion that read nothing has not
# passed.
assert_placeholders_only() {
	local root="$1" files tokens placeholders others leaked
	files="$(find "$root" -name hosts.yml -type f 2>/dev/null | wc -l)"
	if [ "$files" -eq 0 ]; then
		echo "UNKNOWN: no hosts.yml under $root"
		return 1
	fi
	tokens="$(find "$root" -name hosts.yml -type f -exec grep -h 'oauth_token:' {} + 2>/dev/null | awk '{print $2}')"
	placeholders="$(printf '%s\n' "$tokens" | grep -c '^agent-secrets-placeholder-' || true)"
	others="$(printf '%s\n' "$tokens" | grep -v '^agent-secrets-placeholder-' | grep -c . || true)"
	leaked="$(grep -rlE 'gh[opsur]_[A-Za-z0-9]|github_pat_' "$root" 2>/dev/null | wc -l)"
	echo "read $files hosts.yml: $placeholders placeholder token lines, $others other token lines, $leaked files holding a GitHub token"
	[ "$placeholders" -gt 0 ] && [ "$others" -eq 0 ] && [ "$leaked" -eq 0 ]
}

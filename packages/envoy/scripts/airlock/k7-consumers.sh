# shellcheck shell=bash
# packages/envoy/scripts/airlock/k7-consumers.sh
#
# The k7 fixtures and Google consumers. airlock-check.sh sources this file for its production
# run, and airlock-check.test.sh sources the same boundary with only network consumers faked.

K7_CONSUMER_SECRET_INPUTS=(AIRLOCK_GCLOUD_SECRET AIRLOCK_GWS_READ_SECRET AIRLOCK_GWS_ADMIN_SECRET)

k7_consumer_broker_name() {
	local upper=ABCDEFGHIJKLMNOPQRSTUVWXYZ digit=0123456789
	[[ $1 =~ ^[$upper][$upper$digit]*(_[$upper$digit]+)*$ ]]
}

k7_consumer_broker_slug() {
	local slug="${1,,}"
	printf '%s' "${slug//_/-}"
}

k7_consumer_require_inputs() {
	local input
	local -a absent=()
	for input in "${K7_CONSUMER_SECRET_INPUTS[@]}" AIRLOCK_GWS_TIER_KEY; do
		if [ -z "${!input:-}" ]; then
			absent+=("$input")
		fi
	done
	if [ "${#absent[@]}" -ne 0 ]; then
		printf 'airlock-check: k7 requires operator inputs: %s\n' "${absent[*]}" >&2
		return 2
	fi
	# The refusal names the input and never repeats its value: an operator who put a credential in
	# the wrong variable would otherwise see it printed.
	for input in "${K7_CONSUMER_SECRET_INPUTS[@]}"; do
		if ! k7_consumer_broker_name "${!input}"; then
			printf 'airlock-check: %s is no broker name: uppercase letters, digits and single underscores, starting with a letter\n' "$input" >&2
			return 2
		fi
	done
}

# k7_consumer_add_fixtures writes the extra rules and fake secrets for k7's consumers. Its caller
# supplies add_fake_secret, AIRLOCK_RULES_FILE, STACK_SECRETS_PREFIX and K7_CONSUMER_WORK_DIR.
k7_consumer_add_fixtures() {
	local d="${K7_CONSUMER_WORK_DIR:?}"
	mkdir -p "$d"
	jq --arg gcloud "$AIRLOCK_GCLOUD_SECRET" --arg read "$AIRLOCK_GWS_READ_SECRET" --arg admin "$AIRLOCK_GWS_ADMIN_SECRET" '.secrets += [
	     {"name": "SLACK_BOT_TOKEN", "hosts": ["slack.com"]},
	     {"name": "GITHUB_ADMIN_PAT", "hosts": ["api.github.com", "github.com"]},
	     {"name": "TAILSCALE_API_KEY", "hosts": ["api.tailscale.com"]},
	     {"name": $gcloud, "hosts": ["cloudresourcemanager.googleapis.com", "oauth2.googleapis.com", "www.googleapis.com", "cloudkms.googleapis.com"]},
	     {"name": $read, "hosts": ["www.googleapis.com", "oauth2.googleapis.com"]},
	     {"name": $admin, "hosts": ["admin.googleapis.com"]}
	   ]' "$AIRLOCK_RULES_FILE" >"$d/rules.json"
	K7_SLACK="$(printf '%s%s' xox "b-the-fake-slack-token-$RANDOM$RANDOM")"
	K7_GITHUB="$(printf '%s%s' gh "p_thefakegithubadminpat$RANDOM$RANDOM")"
	K7_TAILSCALE="$(printf '%s%s' tskey "-api-the-fake-tailscale-key-$RANDOM$RANDOM")"
	add_fake_secret "${STACK_SECRETS_PREFIX}slack-bot-token" '{"owner":"legion-daemon","tier":"agent"}' "$K7_SLACK"
	add_fake_secret "${STACK_SECRETS_PREFIX}github-admin-pat" '{"owner":"legion-daemon","tier":"agent"}' "$K7_GITHUB"
	add_fake_secret "${STACK_SECRETS_PREFIX}tailscale-api-key" '{"owner":"legion-daemon","tier":"agent"}' "$K7_TAILSCALE"
	# The three Google logins are minted types: the broker mints each at the stack's stand-in
	# Google token endpoint, so what the consumer holds a placeholder for is a minted value.
	local google login input
	google='{"owner":"legion-daemon","tier":"agent","type":"google-login"}'
	login='{"type":"authorized_user","client_id":"the-stand-in-client","client_''secret":"the-stand-in-client-''secret","refresh_''token":"the-stand-in-refresh-''token"}'
	for input in "${K7_CONSUMER_SECRET_INPUTS[@]}"; do
		add_fake_secret "${STACK_SECRETS_PREFIX}$(k7_consumer_broker_slug "${!input}")" "$google" "$login"
	done
}

# k7_consumer_write_pulumi writes the Pulumi program whose Tailscale provider calls getDevices.
k7_consumer_write_pulumi() {
	local d="${K7_CONSUMER_WORK_DIR:?}/pulumi"
	mkdir -p "$d/state"
	cat >"$d/Pulumi.yaml" <<'EOF'
name: airlock-k7
runtime: yaml
description: one tailscale:getDevices call, which the real Tailscale API refuses a fake key
variables:
  devices:
    fn::invoke:
      function: tailscale:index:getDevices
outputs:
  devices: ${devices}
EOF
}

# k7_consumer_preamble <work dir> prints the generated step script's first lines: the names the
# Google steps read each login by, mapped from the operator's inputs, the consumer checkout and
# the boundary itself, then the setup. k7_steps writes it into steps.sh, and the test runs it.
k7_consumer_preamble() {
	local boundary="${BASH_SOURCE[0]}"
	printf 'K7_CONSUMER_WORK_DIR=%q\n' "$1"
	printf 'K7_CONSUMER_GCLOUD_SECRET=%q\n' "$AIRLOCK_GCLOUD_SECRET"
	printf 'K7_CONSUMER_GWS_READ_SECRET=%q\n' "$AIRLOCK_GWS_READ_SECRET"
	printf 'K7_CONSUMER_GWS_ADMIN_SECRET=%q\n' "$AIRLOCK_GWS_ADMIN_SECRET"
	printf 'K7_CONSUMER_TIER_KEY=%q\n' "$AIRLOCK_GWS_TIER_KEY"
	printf 'K7_DOTFILES=%q\n' "${K7_DOTFILES:?}"
	printf 'source %q\n' "$(cd "$(dirname "$boundary")" && pwd)/$(basename "$boundary")"
	printf 'k7_consumer_prepare_steps\n'
}

# k7_consumer_prepare_steps is the generated script's setup: its output file, dotfiles location,
# PATH and isolated gh configuration. The test calls it before the same Google consumer routine.
k7_consumer_prepare_steps() {
	local d="${K7_CONSUMER_WORK_DIR:?}"
	: >"$d/out"
	export DOTFILES_DIR="${K7_DOTFILES:?}"
	export PATH="$K7_DOTFILES/scripts:$K7_DOTFILES/shims:$PATH"
	export XDG_CONFIG_HOME="$d/xdg"
	mkdir -p "$XDG_CONFIG_HOME"
}

k7_consumer_say() {
	printf '%s\n' "$1" >>"${K7_CONSUMER_WORK_DIR:?}/out"
}

# k7_consumer_google_steps runs the commands that read the operator's Google login, read-tier and
# administrator login. K7_CONSUMER_* names are emitted by k7_steps before this file is sourced.
k7_consumer_google_steps() {
	local d="${K7_CONSUMER_WORK_DIR:?}"
	local gcloud_secret="${K7_CONSUMER_GCLOUD_SECRET:?}"
	local read_secret="${K7_CONSUMER_GWS_READ_SECRET:?}"
	local admin_secret="${K7_CONSUMER_GWS_ADMIN_SECRET:?}"
	local tier_key="${K7_CONSUMER_TIER_KEY:?}"

	# A gRPC client: Pulumi's gcpkms secrets manager, which calls Cloud KMS over gRPC alone and
	# offers h2 in its TLS handshake. The airlock carries it in HTTP/2 and Cloud KMS refuses the
	# fake value with gRPC's UNAUTHENTICATED; a proxy without HTTP/2 fails the handshake instead.
	local result
	result="$(cd "$d/pulumi" && export PULUMI_BACKEND_URL="file://$d/pulumi/state" GOOGLE_OAUTH_ACCESS_TOKEN="${!gcloud_secret}" && timeout 120 pulumi stack init k7kms --non-interactive --secrets-provider gcpkms://projects/airlock-k7/locations/global/keyRings/airlock-k7/cryptoKeys/airlock-k7 2>&1 | grep -iE 'error|unauthenticated|deadline|application protocol' | head -n 1 | tr -d '\r')"
	k7_consumer_say "kms=$result"

	# gcloud-broker with gcloud projects list. It reads the login under the operator's name for it.
	result="$(GCLOUD_BROKER_INNER=1 bash "$K7_DOTFILES/scripts/gcloud-broker" -- gcloud projects list --format=json 2>&1 | grep -iE 'ERROR|reason:' | head -n 1 | tr -d '\r')"
	k7_consumer_say "gcloud=$result"

	# One Google Workspace call under the operator's read tier: that tier's command from accounts.json.
	result="$(jq -r --arg key "$tier_key" '.[$key].tiers.read.command | @tsv' "$K7_DOTFILES/gws/accounts.json" 2>&1 | tr '\t' ' ')"
	k7_consumer_say "gws_tier=$result"
	# shellcheck disable=SC2016 # ${!1} expands in bash -c, after secret-run supplies the login.
	result="$(secret-run --reason 'gws operator read' "$read_secret" -- bash -c 'printf "Authorization: Bearer %s" "${!1}" | curl -sS -o /dev/null -w %{http_code} -H @- "https://www.googleapis.com/drive/v3/about?fields=user"' gws "$read_secret" 2>&1 | tail -n 1 | tr -d '\r')"
	k7_consumer_say "gws=$result"

	# The Google Workspace administrator call's domain is a placeholder; the fake token is refused
	# before the service reads the query.
	result="$(printf 'Authorization: Bearer %s' "${!admin_secret}" | curl -sS -o /dev/null -w '%{http_code}' -H @- 'https://admin.googleapis.com/admin/directory/v1/users?domain=example.com' 2>&1 | tail -n 1)"
	k7_consumer_say "admin=$result"
}

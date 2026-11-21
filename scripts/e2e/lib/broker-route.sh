#!/usr/bin/env bash
# Prints the `runtime.kubernetes.agent_secrets` block of a stage proof whose daemon runs the
# Kubernetes runtime (claude-backend-sandbox.sh, stage4b-sandbox-tree.sh), from the operator's
# inputs, once each is one the daemon's loader takes. The block is the pods' broker route: the
# daemon enrols every pod with the deployment's secrets broker, logging in with its own projected
# ServiceAccount token (token_file), and every pod carries a second projected token for the
# broker's exchange (exchange_audience), which each role trades for a broker token naming its own
# session (AGENT_SECRETS_MACHINE_IDENTITY=kubernetes). That token is a session's one credential at
# the Envoy listener: the listener takes no shared token, and a daemon under the Kubernetes runtime
# refuses envoy_token_file.
#
#   LEGION_E2E_AGENT_SECRETS_URL                the broker, an https:// URL with no path (required)
#   LEGION_E2E_AGENT_SECRETS_TOKEN_FILE         an absolute path to a file only its owner can read,
#                                               holding the daemon's own projected ServiceAccount
#                                               token for the broker's audience, an account the
#                                               broker's BROKER_SERVICE_LOGINS names, live for the
#                                               whole run (required)
#   LEGION_E2E_AGENT_SECRETS_AUDIENCE           that token's audience, agent_secrets.audience
#                                               (optional; the loader's default, agent-secrets)
#   LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE  the audience of the pods' exchange token, the
#                                               broker's BROKER_EXCHANGE_K8S_AUDIENCE (required)
#
#   block=$(bash scripts/e2e/lib/broker-route.sh [TOKEN_FILE_VARIABLE...])
#
# Each TOKEN_FILE_VARIABLE named is another token file the proof needs, checked as the daemon's is
# and printed nowhere (claude-backend-sandbox.sh's LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE,
# its scratch listener's own token for the broker's introspection). Stdout is the block, indented to
# sit under `kubernetes:`. A refusal names every variable that is missing or wrong on stderr, in
# one run, never what a variable or a file holds, prints nothing on stdout, and exits 1, so a proof
# calls it in prerequisites, before it creates anything.
set -euo pipefail

refusals=()
refuse() { refusals+=("$1 $2"); }

# token_file NAME checks the token file the variable NAME names: an absolute path, a readable
# regular file (through a symlink, the file it names), no permission bit for its group or others,
# and a token in it.
token_file() {
  local name=$1 path=${!1:-} mode
  if [ -z "$path" ]; then
    refuse "$name" "is unset: it names the file holding a projected ServiceAccount token for the broker's audience"
    return 0
  fi
  [[ $path == /* ]] || {
    refuse "$name" "is not an absolute path"
    return 0
  }
  [[ $path =~ ^/[A-Za-z0-9._/@+-]+$ ]] || {
    refuse "$name" "holds a character a configuration path may not: letters, digits and ._/@+- alone"
    return 0
  }
  [ -f "$path" ] && [ -r "$path" ] || {
    refuse "$name" "is not a readable file"
    return 0
  }
  mode=$(stat -L -c %a -- "$path")
  case $mode in
    *00) ;;
    *)
      refuse "$name" "is readable by its group or others (mode $mode): make it 0600"
      return 0
      ;;
  esac
  [ -n "$(tr -d '[:space:]' <"$path")" ] || refuse "$name" "holds no token"
}

# one_audience NAME VALUE checks one audience: unset is the caller's to judge, white space is refused.
one_audience() {
  [[ $2 =~ ^[A-Za-z0-9._:/-]+$ ]] || refuse "$1" "is not one audience: letters, digits and ._:/- alone"
}

url=${LEGION_E2E_AGENT_SECRETS_URL:-}
if [ -z "$url" ]; then
  refuse LEGION_E2E_AGENT_SECRETS_URL "is unset: it names the deployment's secrets broker, which enrols the run's pods"
elif ! [[ $url =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/?$ ]]; then
  refuse LEGION_E2E_AGENT_SECRETS_URL "is not an https:// URL naming a host and no path, as runtime.kubernetes.agent_secrets.url must be"
fi
url=${url%/}
token_file LEGION_E2E_AGENT_SECRETS_TOKEN_FILE
audience=${LEGION_E2E_AGENT_SECRETS_AUDIENCE:-}
[ -z "$audience" ] || one_audience LEGION_E2E_AGENT_SECRETS_AUDIENCE "$audience"
exchange=${LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE:-}
if [ -z "$exchange" ]; then
  refuse LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE "is unset: it names the audience of the pods' exchange token, the broker's BROKER_EXCHANGE_K8S_AUDIENCE"
else
  one_audience LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE "$exchange"
  [ "$exchange" != "${audience:-agent-secrets}" ] ||
    refuse LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE "must differ from the enrolment's audience (LEGION_E2E_AGENT_SECRETS_AUDIENCE, default agent-secrets): the cluster projects one token per audience"
fi
for name in "$@"; do
  token_file "$name"
done

if [ "${#refusals[@]}" -gt 0 ]; then
  printf 'broker-route: %s\n' "${refusals[@]}" >&2
  exit 1
fi
printf '    agent_secrets:\n      url: %s\n      token_file: %s\n' "$url" "$LEGION_E2E_AGENT_SECRETS_TOKEN_FILE"
[ -z "$audience" ] || printf '      audience: %s\n' "$audience"
printf '      exchange_audience: %s\n' "$exchange"

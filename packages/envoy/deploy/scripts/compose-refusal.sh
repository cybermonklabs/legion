# shellcheck shell=bash
# Sourced by autodeploy.sh and up-dispatch.sh, which read compose/.env only through Compose.

# compose_refusal ERRFILE STATUS COMMAND prints why Compose refused the configuration with no text
# of Compose's own: its error output (ERRFILE) can begin with a warning, and can quote a line of the
# .env whole, a token or a password with it, so nothing of it is printed. What is printed is the
# names Compose gives in its `required variable <NAME> is missing a value` lines, which are names
# alone; and otherwise a fixed sentence with Compose's exit status (STATUS) and the command
# (COMMAND) that shows its message to whoever may read the file.
compose_refusal() {
  local line
  local -a missing=()
  while IFS= read -r line; do
    if [[ "$line" =~ required\ variable\ ([A-Za-z_][A-Za-z0-9_]*)\ is\ missing\ a\ value ]] &&
      [[ " ${missing[*]} " != *" ${BASH_REMATCH[1]} "* ]]; then
      missing+=("${BASH_REMATCH[1]}")
    fi
  done <"$1"
  if ((${#missing[@]})); then
    printf 'a required variable is missing a value: %s' "${missing[*]}"
  else
    printf '%s' "docker compose config exited $2; its message is not shown, as it can quote a setting's value; run \`$3\` to read it"
  fi
}

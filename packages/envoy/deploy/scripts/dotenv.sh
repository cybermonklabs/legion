# shellcheck shell=bash
# Sourced by autodeploy.sh and up-dispatch.sh, which check compose/.env before they start Dispatch.

# dotenv_value NAME FILE prints the value Docker Compose reads for NAME from the dotenv file FILE,
# as Compose v5 reads it (measured with v5.0.1 and v5.7.0 through `docker compose config`):
# - the last line that sets NAME, with or without an `export ` prefix, with blanks allowed before
#   the name and around the `=`;
# - its value with leading blanks dropped, then either one pair of matching surrounding double or
#   single quotes dropped, the text inside kept as written (a ` #` included), or, unquoted,
#   everything from the first ` #` (a space, then `#`) on cut and trailing blanks dropped.
# So `NAME=""` and `NAME=''` are empty, `NAME=v # note` is `v`, `NAME= # note` is `# note`, and a
# tab before `#` cuts nothing, as in Compose. It prints nothing when FILE does not exist or does
# not set NAME.
dotenv_value() {
  local name=$1 file=$2 line value='' quote rest
  [[ -f "$file" ]] || return 0
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_.]*)[[:space:]]*=(.*)$ ]] &&
      [[ "${BASH_REMATCH[2]}" == "$name" ]]; then
      value=${BASH_REMATCH[3]}
    fi
  done <"$file"
  value=${value#"${value%%[![:space:]]*}"}
  quote=${value:0:1}
  if [[ "$quote" == '"' || "$quote" == "'" ]]; then
    rest=${value:1}
    if [[ "$rest" == *"$quote"* ]]; then
      printf '%s\n' "${rest%%"$quote"*}"
      return 0
    fi
  fi
  value=${value%% #*}
  value=${value%"${value##*[![:space:]]}"}
  printf '%s\n' "$value"
}

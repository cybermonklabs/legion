# Check a value without printing it

Read this before you check a value's shape, or take a credential out of a JSON value.

- Check a plain value's shape by its length and its prefix alone:

  ```sh
  agent-secrets NAME --reason 'check the shape of this credential' -- sh -c 'echo "${#NAME}"; case "$NAME" in ghp_*) echo prefix-ok;; *) echo prefix-no;; esac'
  ```

- Check a JSON value by its key paths and the length of each leaf, never by printing a leaf, masked or not. `try … catch` keeps jq's own parse error, which quotes the value, out of the output:

  ```sh
  agent-secrets NAME --reason 'check the shape of this credential' -- \
    jq -n 'env.NAME | try fromjson catch error("the value is not JSON") | [paths(type != "object" and type != "array") as $p | {path: ($p | join(".")), length: (getpath($p) | tostring | length)}]'
  ```

- Take a credential out of a JSON value inside the command that uses it. `jq -e` stops the command rather than send an empty credential:

  ```sh
  agent-secrets NAME --reason '…' -- sh -c '
    token=$(printf %s "$NAME" | jq -er ".path.to.token" 2>/dev/null) || { echo "no token at .path.to.token" >&2; exit 1; }
    printf "Authorization: Bearer %s" "$token" | curl -sS -H @- https://api.example.com/'
  ```

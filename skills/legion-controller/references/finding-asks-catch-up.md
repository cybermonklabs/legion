# The catch-up reads

Read this at steps 3 and 4 of `skill://legion-controller/references/finding-asks.md#catch-up-at-every-start`: each step runs its read below, and that step says what to do with what it lists.

## Step 3: the answered asks still waiting on you

Run it for the report issue, then for each issue of the `needs_review` snapshot, with that issue's key as `<ISSUE KEY>`:

```text
asks=$(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")" | curl -sS --fail-with-body -H @- "$DISPATCH_URL/api/v1/issues/<ISSUE KEY>/asks?state=answered&thread=1") || { echo "Dispatch read failed" >&2; exit 1; }
printf '%s' "$asks" | jq -cn 'input | if type == "array" then [.[] | select(any((.options // [])[]; .label == "Do not file" or .label == "Admit it" or .label == "Park it" or .label == "Leave it admitted" or .label == "Defer it" or .label == "Let it run")) | .answer.at as $t | select([.thread.replies[]? | select(.author.kind == "session" and .created_at > $t and (.body | test("^(Filed dispatch://|Handed dispatch://|Admitted dispatch://|Not filed:|Not admitted:|Parked dispatch://|Not parked dispatch://|Left admitted dispatch://|Deferred dispatch://|Not deferred dispatch://|Let run dispatch://)")))] | length == 0) | {id, question, answer}] else error("Dispatch did not answer the list") end'
```

## Step 4: the clear defects nobody took yet

```text
notes=$(printf 'Authorization: Bearer %s' "$(< "$DISPATCH_TOKEN_FILE")" | curl -sS --fail-with-body -H @- "$DISPATCH_URL/api/v1/issues/<REPORT KEY>/comments") || { echo "Dispatch read failed" >&2; exit 1; }
printf '%s' "$notes" | jq -cn 'input | if type == "array" then (. as $all | [.[] | select(.reply_to == null and (.body | startswith("Clear defect from "))) | .id as $id | select([$all[] | select(.reply_to == $id and .author.kind == "session" and (.body | test("^(Handed dispatch://|Filed dispatch://|Not filed:|Asked dispatch://|Moved dispatch://)")))] | length == 0) | {id, body}]) else error("Dispatch did not answer the list") end'
```

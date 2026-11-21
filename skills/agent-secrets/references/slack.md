# Slack

Read this before you read from Slack or post to it.

Slack's read methods go through `curl` under this client.
Every post to Slack is a call of the Envoy tool `slack_post`, which holds the reply to one thread, to 4,000 characters and to one send.

## A worked example: one page of a Slack thread

```sh
agent-secrets LEGION_SLACK_BOT_TOKEN --reason 'read the thread of a Slack mention' --wait 0s -- sh -c '
  printf "Authorization: Bearer %s" "$LEGION_SLACK_BOT_TOKEN" |
  curl -sS -G https://slack.com/api/conversations.replies \
    -H @- \
    --data-urlencode channel=<channel_id> \
    --data-urlencode ts=<thread_ts> \
    --data-urlencode limit=200'
```

It exits 0 and answers `"ok": true` with the messages of the thread.
A `ts` that names no message answers `{"ok":false,"error":"thread_not_found"}`, also at exit 0, which is why the body decides and not the status.
A thread read needs the app's `channels:history` scope in a public channel and `groups:history` in a private one; without them Slack answers `missing_scope`, which only a person in the Slack workspace can fix.
`LEGION_SLACK_BOT_TOKEN` is a shared agent secret, so every session with a broker identity is granted it; a process with no broker identity exits 1.

Paging: Slack recommends a `limit` of at most 200.
While the answer's `has_more` is true, pass its `response_metadata.next_cursor` as `cursor` to the next call.
Each page repeats the thread's root message, so drop repeats by `ts` and sort by `ts`.

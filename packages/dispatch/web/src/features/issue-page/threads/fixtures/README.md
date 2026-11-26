# Threads fixtures

Answers of the real Dispatch server, captured whole for the unit tests of Threads
(dispatch://INFRA-5704). The unit tests read server shapes only
from these files. Do not edit a file by hand: capture it again with its command below.

- **Commit**: `d8c74fdb236d8b9080defc9eadd4dcdab3db80ac` (the stack base B), with the seed
  `seedThreads({ second: true })` of `packages/dispatch/e2e/issue-page-threads.ts` added in the
  working copy.
- **Time**: 2026-11-26T11:39:07Z, as `date -u +%FT%TZ` printed it before the first read.
- **Format**: each answer as the server sent it, formatted by `bunx biome format --write` (white
  space only).

## How they were captured

From `packages/dispatch`, one stack, as `e2e/harness-up.ts`'s header says, with its exports file
under `$TMPDIR`, made 0600 and deleted after:

```sh
rm -f "$TMPDIR/threads-exports.sh"
(umask 077 && exec bun e2e/harness-up.ts > "$TMPDIR/threads-exports.sh") &
# …once the file ends with `export DISPATCH_E2E_REUSE_SERVERS=1`:
. "$TMPDIR/threads-exports.sh"
```

A script under `$TMPDIR` then ran `resetDatabase()` (`e2e/seed.ts`) and `seedThreads({ second:
true })`, and took these variables from the seed's answer (`ThreadsSeed`):

| Variable | The seed's field |
| --- | --- |
| `KEY` | `key` |
| `SECOND_KEY` | `second.key` |
| `ASK_ID` | `asks.open` |
| `HEAD_ID` | `comments.remark` |
| `QUOTE_REPLY_ID` | `comments.postQuote` |
| `REPLY_TO_ID` | `comments.post` (the `reply_to` of `comments.postQuote`) |
| `ROOT_ID` | `messages.first` |
| `MESSAGE_REPLY_ID` | `messages.firstAgentReply` |
| `NOBODY` | `00000000-0000-4000-8000-000000000000`, an id that names nothing |

The reader signed in as `e2e/users.ts` signs in, at the dev sign-in route, with the cookie kept
under the same scratch directory, and so did a fresh person for `me-state-fresh.json`:

```sh
curl -sS -c "$COOKIE" -o /dev/null "http://127.0.0.1:$DISPATCH_E2E_PORT/auth/_dev/signin?login=alice"
curl -sS -c "$FRESH" -o /dev/null "http://127.0.0.1:$DISPATCH_E2E_PORT/auth/_dev/signin?login=carol"
```

Each read below is `curl -sS -b "$COOKIE" -o "<file>" "http://127.0.0.1:$DISPATCH_E2E_PORT<path>"`
(`-b "$FRESH"` for `me-state-fresh.json`), so the table gives the path. The stack was stopped by
its process id once the last answer was read, and the scratch directory deleted.

## The files

| File | Path | Shape |
| --- | --- | --- |
| `comments-in.json` | `/api/v1/issues/$KEY/comments?order=activity&person=in&limit=20` | `CommentThreadPage`: 20 threads, `before` set, `remaining` 2. Holds the comment thread on the spec's text with the reviewer's reply that quotes the planner's reply (`reply_block.quote`), which joins that record on text. |
| `comments-in-before.json` | the same, with `&before=<comments-in.json's before>` | `CommentThreadPage`: the last 2 threads, `before` null, `remaining` 0. Holds the post with the planner's reply that quotes its second block (`reply_block.quote`) and the comment that answers that reply, and the reader's deleted reply. |
| `comments-in-author.json` | `/api/v1/issues/$KEY/comments?order=activity&person=in&limit=20&author=alice` | `CommentThreadPage`: the 17 comment threads the reader wrote in. |
| `messages-in.json` | `/api/v1/issues/$KEY/messages?order=activity&person=in&limit=20` | `MessageThreadPage`: 16 threads. Holds the tester's reply that answers block `b2` of the reader's message with a quote (`in_reply_to_block`, `reply_block.quote`), the agent's note (a root with no target), and no thread the Legion daemon began. |
| `messages-in-author.json` | the same, with `&author=alice` | `MessageThreadPage`: the 14 threads the reader wrote in. |
| `messages-none.json` | `/api/v1/issues/$KEY/messages?order=activity&person=none&limit=20` | `MessageThreadPage`: the thread between agents alone. |
| `messages-none-author.json` | the same, with `&author=alice` | `MessageThreadPage`: no thread. |
| `asks-thread.json` | `/api/v1/issues/$KEY/asks?order=activity&thread=1&limit=20` | `AskThreadPage` whose rows carry `thread`: 12 asks, the open ask that waits on the reader among them. |
| `asks-thread-author.json` | the same, with `&author=alice` | `AskThreadPage`: the 12 asks the reader answered or replied in. |
| `comments-bare.json` | `/api/v1/issues/$KEY/comments` | The bare list an older build answers: an array of 37 comments. |
| `messages-bare.json` | `/api/v1/issues/$KEY/messages` | The bare list: an array of 18 `MessageRead`. |
| `asks-bare.json` | `/api/v1/issues/$KEY/asks` | The bare list: an array of 12 `IssueAskRow`. |
| `events.json` | `/api/v1/issues/$KEY/events?order=desc&limit=200` | An array of the seeded issue's 112 events, newest first. |
| `events-past-200.json` | `/api/v1/issues/$SECOND_KEY/events?order=desc&limit=200` | An array of 200 events of the second issue, newest first, every one past the reader's `last_read_seq` of that issue (2, in `me-state.json`): one `message.answered` in the note's thread, which the reader did not write in, and 199 `comment.created`. |
| `replies-unread.json` | `/api/v1/me/replies?issue=$KEY&state=unread` | `MyRepliesResponse`: 6 rows, `total` 6. One row's `root_author` is the Legion daemon's session (`legion-daemon:<PROJECT>`), and two rows are threads of two records (the post, and the reader's message the tester quoted). |
| `replies-limit-200.json` | `/api/v1/me/replies?issue=$KEY&limit=200` | `MyRepliesResponse`: the same 6 rows, as the read mark's read sends it. |
| `me-state.json` | `/api/v1/me/state` | `UserState` holding both issues' state, `last_read_seq` among it. |
| `me-state-fresh.json` | `/api/v1/me/state`, as a fresh person | `UserState` holding no issue: `{}`. |
| `ask.json` | `/api/v1/asks/$ASK_ID` | `AskRead` of the open ask, with its replies. |
| `comment-head.json` | `/api/v1/comments/$HEAD_ID` | `CommentRead` of a thread's head: the reader's remark with its replies. |
| `comment-quote-reply.json` | `/api/v1/comments/$QUOTE_REPLY_ID` | `CommentRead` of the reply with a quote: the reply alone, `replies` empty, since a reply stores its thread's head in `reply_to`. |
| `comment-reply-to.json` | `/api/v1/comments/$REPLY_TO_ID` | `CommentRead` of that reply's `reply_to`: the post with every reply of its thread. |
| `message-root.json` | `/api/v1/messages/$ROOT_ID` | `MessageRead` of the first message thread, read by its root. |
| `message-reply.json` | `/api/v1/messages/$MESSAGE_REPLY_ID` | `MessageRead` of the same thread, read by a reply: the whole conversation. |
| `ask-404.json` | `/api/v1/asks/$NOBODY` | `404`: `ErrorBody` (`code`, `error`). |
| `comment-404.json` | `/api/v1/comments/$NOBODY` | `404`: `ErrorBody`. |
| `message-404.json` | `/api/v1/messages/$NOBODY` | `404`: `ErrorBody`. |
| `message-created.json` | `POST /api/v1/issues/$KEY/messages` | The `201`: the stored root message with `target` `role:planner` and its first delivery attempt `aside`, and `advice`. |

The write of `message-created.json` was sent last, after every read above:

```sh
curl -sS -b "$COOKIE" -H "Origin: http://127.0.0.1:$DISPATCH_E2E_PORT" -H "Content-Type: application/json" \
  -X POST --data '{"body":"A new thread to the planner.","delivery":"aside","target":"role:planner"}' \
  -o message-created.json "http://127.0.0.1:$DISPATCH_E2E_PORT/api/v1/issues/$KEY/messages"
```

## The event types the events answers hold

Which events are writes follows the server (R7): an agent's reply message is
`message.answered`, a delivered comment reply is `comment.answered`, and `ask.answered` fires on
each change of the answer. An edit, a delivery and `message.accepted` name the same ids and are no
writes. A write's entry is the one `payload.id` names: a comment's, a message's, or an ask's id.

| Event type | In | Count | A write | The entry it names |
| --- | --- | --- | --- | --- |
| `issue.created` | `events.json` | 1 | No | None: it names the issue. |
| `comment.created` | `events.json`, `events-past-200.json` | 37, 199 | Yes | The comment, `payload.id`; a reply carries `reply_to` (the thread's head), `answers` and `thread_root_id`, and an ask's comment `ask_id`. |
| `comment.deleted` | `events.json` | 1 | No | The comment withdrawn, `payload.id`: it adds no entry. |
| `message.created` | `events.json` | 18 | Yes | The root message, `payload.id`. |
| `message.answered` | `events.json`, `events-past-200.json` | 13, 1 | Yes | The reply, `payload.id`, with `in_reply_to` the message it answers, not the root. |
| `message.delivery` | `events.json` | 19 | No | None of its own: it names `payload.message_id`, and carries no `payload.id`. |
| `ask.opened` | `events.json` | 12 | Yes | The ask, `payload.id`. |
| `ask.answered` | `events.json` | 11 | Yes | The ask, `payload.id`: the answer it holds. |

The seed writes no `comment.answered`, `comment.edited`, `message.edited`, `message.accepted` or
`ask.resolved`, so no file holds one: the first and the last are writes, and the three between
are not.

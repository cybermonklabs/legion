# The real entries this count's tests read

Each file here is one entry copied word for word from a real session the transcript store holds.
Its first line is the session header, with `copiedFrom`: the host, the session, the file, the line
and the store's load time it came from. No entry here was written by hand, and each was read for
credentials before it was copied.

| File | The entry | Session | Line | The store loaded it |
| --- | --- | --- | --- | --- |
| `real-ask-envoy.jsonl` | the daemon's ask to merge the base forward, as the Envoy renderer delivered it to the implementer | `01a21150-9f6d-7151-8882-ce4b4b4b44fe` (host `legion-infra`) | 5172 | 2026-11-26 04:01:56.501Z |

An earlier copy of this fixture held an ask about a pull request of a repository the leak-term
list names. The list refused that push, naming the term and the line, and this entry replaced it:
an ask about a pull request of this repository. That refusal is the control of the copy, and it
ran before the fixture landed.

**The worker stream holds no such ask.** The daemon publishes the ask to the role's own Envoy topic
and to nothing else (`packages/daemon/internal/record/outbox.go`'s `MergeForwardAsk`, published by
`packages/daemon/internal/daemon/merge_forward.go`), so no task of the worker stream carries it.
Read on 2026-11-26 at 02:30Z, over every host the store holds since 2026-11-01: the ask's own
sentence `a second read of GitHub confirmed it` stands in 2,372 `custom:envoy-message` rows, 427
tool results, 6 assistant messages, 1 `user:agent` row (an IRC relay inside a development session),
and **no `kind = 'user'`, `role = 'user'` row**, which is what a task of the worker stream is. The
control of that reading, run in the same window with the same shape, counts 2,992 worker-stream
task rows holding `Phase: retro`, so the query can see such rows and found none of this ask.

# Asking whether to park a tree

Read this when a wake or the walk sends you a tree whose wait has a cause that stays, and when a person answers a park ask. The tree stays admitted and keeps working while you ask. A comment or reply from any session counts as yours, since your session id changes.

## The key of each wait

Each wait has a fixed `<key>`, the same at every run, and a `<cause>`, its wording today. Build the key exactly from `read_state`:

- an escalated hold: `held <ISSUE> in <heldFrom> (<role>) at <failure.at>`, from `issues.<ISSUE>.heldFrom`, the role that works its place (`issues.<ISSUE>.placeRole`, else that phase's role) and `issues.<ISSUE>.workers.<role>.claim.failure.at`;
- a failed root architect: `architect failed: <cause word> at <failure.at>`, from `issues.<KEY>.architect.failure`'s `cause` and `at`;
- an event outside Legion: `event: <the event>` (below).

With no `failure.at`, end the key `generation <n>` (`issues.<ISSUE>.generation`). A tree with an escalated hold or a failed architect takes that key, never an `event:` one.

## Ask

1. Find your earlier ask for this key:

   ```text
   notes=$(dispatch api --path '/api/v1/issues/<root>/comments') || { printf 'Dispatch read failed: %s\n' "$notes" >&2; exit 1; }
   printf '%s' "$notes" | jq -cn 'input | if type == "array" then [.[] | select(.author.kind == "session" and (.body | startswith("Asked whether to park: "))) | {id, created_at, body}] else error("Dispatch did not answer the list") end'
   ```

   Take the newest whose second line is exactly `<key>`, and `dispatch read --ref` the ask its first line names. Open, resolved with no answer, or your reply under it starts `Left admitted`: do nothing. Answered with no reply of yours: step 4. Your reply starts `Parked` or `Not parked`: the ask is spent; go on to step 2. No such comment: step 2.
2. Open a `dispatch ask` on the report issue naming the root and `<cause>`, options `Park it` and `Leave it admitted`, saying that after a park Legion sets the root back to `todo` when the wait ends. Follow it (`dispatch follow --ask '<ref>' --action follow`). Comment on the root (`dispatch comment --issue <root> --body-file -`): first line `Asked whether to park: dispatch://<REPORT KEY>/ask/<id>`, second line `<key>`, then `<cause>`.
3. Name the tree and the ask in the day's report while it is open.
4. On `Park it`, while the root's `phase` is not `done` and `read_state` still builds the same `<key>`: park the tree, with a comment citing the answer whose first line is `Parked by Legion while it waits on: <cause>`, the mark that makes the park your own hold. Otherwise, or on `Leave it admitted`, move nothing. Then reply under the ask, first line exactly `Parked dispatch://<root>`, `Not parked dispatch://<root>` or `Left admitted dispatch://<root>` (`dispatch comment --issue <REPORT KEY> --reply-to-ask '<ask id>' --body-file -`). Dispatch cannot resolve an answered ask, so that reply is its record.

Set a root parked with that mark back to `todo` once its cause is gone or its event has happened, with a comment on the root citing the mark and what you checked; or when a person asks, or a person's decision after the park gives it a next step (`skill://legion-controller/references/parked-decisions.md`).

## A tree that waits on an event outside Legion

A wait that a `blocked_by` link names gives its slot back by itself: there is nothing to ask. For any other event outside Legion (a deploy, another project's release, a vendor, a person outside the company) the tree keeps its slot: ask as above, key `event: <the event>`. The event is one fixed reference: a `dispatch://` reference, or a pull request, release or run URL, or the message that names what the tree waits on. Choose it at the first ask, and copy it from that ask's comment after.

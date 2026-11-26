# Asking whether to park a tree

The tree stays admitted and keeps working while you ask. A comment or reply from any session counts as yours, because your session id changes.

## The key of each wait

Each wait has a fixed `<key>`, the same at every run, and a `<cause>`, its wording today. Build the key exactly from `read_state`:

- an escalated hold: `held <ISSUE> in <heldFrom> (<role>) at <failure.at>`, from `issues.<ISSUE>.heldFrom`, that phase's role and `issues.<ISSUE>.workers.<role>.claim.failure.at`
- a failed root architect: `architect failed: <cause word> at <failure.at>`, from the `cause` and `at` of `issues.<KEY>.architect.failure`
- an event outside Legion: `event: <the event>` ([below](#a-tree-that-waits-on-an-event-outside-legion))

With no `failure.at`, end the key `generation <n>` (`issues.<ISSUE>.generation`). A tree with an escalated hold or a failed architect takes that key, never an `event:` key.

## Ask

1. Find your earlier ask for this key: the newest comment on the root, by a session, whose first line is `Asked whether to park: <ask reference>` and whose second line is exactly `<key>`. `dispatch read --ref` the ask it names.
   - The ask is open, or resolved with no answer, or your reply under it starts `Left admitted`: do nothing.
   - The ask is answered and carries no reply of yours: go to step 4.
   - Your reply under it starts `Parked` or `Not parked`, or no such comment exists: go to step 2.
2. Open a `dispatch ask` on the report issue. Name the root and `<cause>`. Give the options `Park it` and `Leave it admitted`. Say that after a park Legion sets the root back to `todo` when the wait ends. Follow the ask (`dispatch follow --ask '<ref>' --action follow`). Comment on the root: first line `Asked whether to park: dispatch://<REPORT KEY>/ask/<id>`, second line `<key>`, then `<cause>`.
3. Name the tree and the ask in the day's report while the ask is open.
4. On `Park it`, while the root's `phase` is not `done` and `read_state` still builds the same `<key>`: `set_status` the root to `backlog`. Comment on the root, citing the answer, with the first line `Parked by Legion while it waits on: <cause>`. That mark makes the park your own hold. Otherwise, and on `Leave it admitted`, move nothing. Then reply under the ask with the first line `Parked dispatch://<root>`, `Not parked dispatch://<root>` or `Left admitted dispatch://<root>`.

Set a root parked with that mark back to `todo` once its cause is gone or its event has happened. Comment on the root with the mark and what you checked.

## A tree that waits on an event outside Legion

A wait that a `blocked_by` link names gives its slot back by itself: ask nothing. For any other event outside Legion (a deploy, another project's release, a vendor, a person outside the company), the tree keeps its slot. Ask as above, with the key `event: <the event>`. The event is one fixed reference: a `dispatch://` reference, a pull request, release or run URL, or the message that names what the tree waits on. Choose it at the first ask. Copy it from that ask's comment after.

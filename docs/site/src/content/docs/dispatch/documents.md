---
title: Documents
description: Specs and other documents, versions, margin comments, suggestions, decision blocks, approvals, block links, and search.
sidebar:
  order: 3
---

Every issue has one main document, its **spec**, which the issue page draws under the tree on the
issue's own address and on the Spec preset. An issue can hold other documents too: the tree draws
each one a block cites under that block, the **Files and documents** menu beside the presets lists
the others, and `/issues/<KEY>/artifacts` lists them all. A project can hold documents that belong
to no single issue, on the project's Documents tab. All of them work the same way.

![An issue's spec in the editor, with a decision block in the document and a comment thread in the margin.](/legion/media/dispatch/issue-spec.png)

## Editing

A document is live: you and everyone else with it open edit the same text at the same time, and
see each other's changes as they happen. There is no save button. Dispatch saves what you type as
you go.

A dot in the document's toolbar or header shows its connection: connecting, connected, offline,
or failed. A screen reader reads which. Pointing at the dot shows it too, except on the issue page,
which shows no tooltips. A document left open stays connected while nobody types in it: the page
renews its presence every 15 to 18 seconds and Dispatch answers each renewal (a page that can only
read gets an empty answer, since it cannot change who is shown present), so neither the page nor a
proxy in front of Dispatch takes the quiet connection for a dead one.

## Versions

When the text settles, about two seconds after a change, Dispatch records a new version. The
controls in the document's toolbar work with them. On the issue page that toolbar is at the end of
the preset row, and under the tree on a screen narrower than 768 px:

- **Version** picks a version to read. An older version is read-only.
- **Name version**, which reads **Name** in the issue page's toolbar, records the current text as
  a version and asks **What changed?** for a short description.
- **Diff vs current** appears while you read an older version, and shows how it differs from the
  current text.

Other documents carry the same controls in their header.

Every version records how big it is stored: a document's markdown in bytes, as a file's are. The
issue's list at `/issues/<KEY>/artifacts` and the project's Documents tab show each document's
latest size beside its kind, and a document's version list shows every version's, so you can find
a document close to the 1 MiB limit without opening it.

## Comments in the margin

Comments on a document sit in the margin beside the text they are about. On a wide screen the
margin is a column on the right; `Shift+M` hides and shows it. On a narrower screen it opens as a
sheet from the bottom.

![A comment thread in the margin, next to its highlighted text](/legion/media/dispatch/margin-thread.png)

To comment, select some text. A bar appears with three actions:

- **Comment** opens a comment on the selection. `Ctrl+Alt+M` (`⌘+⌥+M` on a Mac) does the same
  from the keyboard.
- **Suggest** proposes replacement text for the selection.
- **Ask** opens a question on the selection, with an urgency, options, and **Allow multiple**.

You can switch between the three in the composer before you send. The selection is highlighted in
the document, and choosing a highlight opens its thread in the margin.

A thread is a card with its replies in order. In a thread you can:

- reply,
- edit your own comments,
- **Resolve** the thread when it is done.

Resolved threads fold into **Resolved (N)**. A resolved thread has **Reopen**. The same comments
also appear in the issue's Conversation, in the issue page's panel, one card per thread, where a
resolved one is listed under **Closed**.

### Suggestions

A suggestion shows the text it would remove and the text it would add. A person decides it:

- **Accept suggestion** applies the change to the document.
- **Reject suggestion** closes it and leaves the text as it was.

Anyone can propose a suggestion, with **Suggest** or through an agent's tools. Only a person can
accept or reject one.

## Decision blocks

A decision block is a question written into the document itself, at the end of the section it is
about. It shows as a card with its urgency and the word **Decision**, and its options as rows you
can pick. It is an ask like any other: it appears in your Inbox too, and an answer or **Ask back**
made in either place shows in both.

To edit a decision's options, click into its question. The option list appears for that block,
and folds away again when you click elsewhere.

When a document has open decisions, a line above its text counts them and links one of them. With
more than one, the arrow at the end of the line, **Next open decision**, links the next one.

## Approvals

An agent can ask you to approve a document at its current version. Approval belongs to a version,
the way a code review belongs to a commit. Once someone has asked, the document header shows a chip
with the approval state:

| Chip | Meaning |
| --- | --- |
| `Awaiting approval` | Approval was requested and nobody has reviewed this version yet. |
| `Approved v12` | Version 12 is approved, and it is the latest version. |
| `Approved v12 · changed since` | Version 12 was approved, then the document changed, other than by your own answer to a decision block (below). |
| `Changes requested` | A reviewer asked for changes. |

Choose the chip to see the review history. Beside it:

- **Approve** approves the latest version. When the approved version is out of date, the button
  names the latest one, for example **Approve v13**.
- **Request changes** asks you for a reason, which goes back to the agent.

The request also reaches your Inbox as an **Approval requested** card. Answering either one is the
same review. When you approved an earlier version of the document, the card puts
`Approved at version N on <date> by <who>.` under the question, with a **Show diff** control that
shows what changed between that version and the one you are asked to approve, with every decision
block left out of both, so you read what changed outside the decisions you already made; the
agent's summary names it in a sentence.

Your own answer to a decision block in a document you approved carries your approval forward when
that answer is the whole change since the version you approved: Dispatch records an approval in
your name at the version your answer writes, the chip reads `Approved` at that version, and the
review history shows it, so nobody asks you to approve again what you just decided. A change
outside the decision blocks, a block added or answered since your approval without your own answer
(someone else's answer, or a block waived), a decided block whose question or options were
changed, a removed block, or a request for changes newer than your approval leaves the document
`changed since`, and the next request shows you the diff.

When Legion's design gate is on, Legion waits for this approval before it builds what a root
issue's spec describes.

## Block links

Every paragraph, list, and other block in a document has a stable link. Put your cursor in a
block and choose **Copy link** in the issue page's document toolbar. The link ends in `#b-` and
the block's id. Opening it scrolls to that block and highlights it briefly. Dispatch gives each
block its id. Markdown an agent writes can name one as `{#id}`. Dispatch stores an id of any
length, but a block whose id is longer than 256 bytes is left out of the links between documents:
text that links it is no link, and nothing lists what cites it. A block link names its block only
while the document's key and name and the block's id, `<KEY>/<name>#<id>`, come to at most 2048
bytes, the bound every reference is held to (References, below); a longer one names no block, and
opening it opens the document.

## References

Anything in Dispatch can be linked with a `dispatch://` reference: `dispatch://CORE-12` for an
issue, `dispatch://CORE-12/spec` for its spec, and similar forms for documents, asks, comments, and
messages. In a document or a comment, a reference shows as a link titled with what it points to.
Rest the pointer on a link, or focus it with the keyboard, to see a preview card. On an issue's
page only the keyboard opens it: focus the link, or put the caret in it in a document you are
editing.

A reference names what it points to only while the id Dispatch records the link under comes to at
most 2048 bytes: an issue's key, a document's `<KEY>/<name>`, a block's `<KEY>/<name>#<id>`, a
comment's or message's block's `comment/<id>#<block>` or `message/<id>#<block>`, or an ask's,
comment's or message's id. A longer one is no link: your text is posted as written. The
bound decides only what links: a document with a longer name is still read, edited and opened as
any other.

An id can be written short: 8 or more of the characters it starts with name the one item that
carries them, on a page address and in a reference alike, and the page then puts the whole id back
in its address, so an address you copy from the page keeps working. A short id two items share
opens a page listing both.

An issue key on its own, such as `CORE-12`, links to that issue once that issue exists, and `#`
with a question block's id links to that question of the document the text belongs to. Both show
their target's title and both join the **Referenced by** list below, as a `dispatch://` reference
does.

To write a literal `#id` in a document rather than a link to that question, put it in a code
span: a document keeps no escape of its own, since it stores `\#q2` as `#q2`, and code shows as
text on every surface.

Every issue, document, and ask has a **Referenced by** list of what links to it. A document page
shows the list open; an issue shows it in its header.

## Search

Press `Ctrl+K` (`⌘K` on a Mac), or choose **Search** in the sidebar, to open the palette. Press `/`
to open it for search alone.

![The search palette with actions for the current page above search results](/legion/media/dispatch/palette.png)

Search covers issue titles, the latest text of every document, comments, asks and their answers,
and messages. A query needs at least two characters and can be up to 1,000. You can use quotes for
a phrase, `OR` between words, and `-` before a word to leave it out: `"merge queue" -draft`.

A very long text made almost entirely of words that each appear once, such as a list of hundreds
of kilobytes of identifiers, holds more words than one search index entry can. Search then indexes
its opening part, the largest of its first half, quarter, eighth and so on that fits, up to the
last whole word in it, and does not index the words after it. When the opening part has no
whitespace before the cut, none of its words are indexed. An issue is always found by its key.

Results are grouped by the issue or document they belong to. Choosing a document result opens the
document and highlights the first place the words appear. Text typed into a live document can take
about two seconds to show up in search.

[Keyboard shortcuts](/legion/dispatch/keyboard/#the-help-and-the-palette) covers the palette's
other modes.

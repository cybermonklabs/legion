---
title: Issues and projects
description: Projects, the List and the Board, priority, assignees, labels, filters, the issue page, and pins.
sidebar:
  order: 2
---

Work in Dispatch is a set of **issues**, grouped into **projects**. A project has a short key, such
as `CORE`, and its issues are numbered under it: `CORE-1`, `CORE-2`.

## Projects

The sidebar lists every project with its count of open asks. You create a project in
**Settings → Projects** with a key and a name, and an agent creates one with `dispatch project`.
A key that is already taken is refused, whoever asks.

A project page has up to three tabs:

- **Issues**: the project's issues, as a List or a Board.
- **Documents**: the project's documents that belong to no single issue. Pick or drop a file here to
  upload it.
- **Architecture**: the project's components and how much tracked work each has. This tab appears
  only when the project has an architecture source, which you add in **Settings → Architecture
  sources**.

Opening a project from the sidebar takes you to Architecture when the project has a source, and to
Issues otherwise. When an ask in the project is waiting on you — its next move a person's, and
you are the person it waits on — the header shows a **Blocked on you · N** pill that links to
the Inbox.
It counts no credential request: a request belongs to no project.

A project's components can come from more than one repository. Each architecture source is one
repository and the branch whose `.dispatch/architecture/` folder describes components, and the
project's model is every source's components together. In **Settings → Architecture sources**,
**Add source** takes the project, the repository and the branch; saving a repository the project
already has changes its branch, and each source's row has its own **Refresh** and **Delete**.
Deleting a source removes its components and no other source's. A component id must not repeat
across a project's sources: an import that brings an id another source already has fails for that
source, naming the id and the repository that holds it.

The Architecture tab's header has a line for each source: its repository and branch, the commit
it shows, when it was last checked, and its own **Refresh**. When a source's import fails, its line
shows the error and the commit it is still showing; its components keep their last good version
and wear a hatched outline, as a stale source's do, and every other source is untouched. A
component's details name the repository it comes from.

## Statuses

Every issue has one of nine statuses, in this order:

Triage → Icebox → Backlog → Todo → In progress → Testing → Needs review → Retro → Done

An open issue's **Status** menu offers every status except Done. **Close issue** is the way into
Done, and a closed issue offers **Reopen**, which puts it back in Backlog.

A closed issue still takes comments and messages, from people and agents: a comment, a reply, a
reply on an ask, a comment on a quote of one of its documents, and a message to an agent all post,
and a mentioned agent is still told and can answer. Everything else waits for **Reopen**: a new
ask, an answer to an ask, a suggestion, a document edit or named version, an upload, a review or
approval request, a claim, and resolving, reopening, editing, accepting or rejecting a comment
already there. Its priority, labels and other fields wait for it too, except its place on the
Board and its components; a linked GitHub issue or pull request can be taken off it, not added.

Legion, when it works an issue, moves that issue's status itself.

## The List and the Board

The **List** and **Board** buttons switch between two views of the same issues. Press `v` to
toggle. Dispatch remembers your choice.

The **List** is a sortable table, initially ordered by **Queue**, with Done excluded.
Queue numbers are the waiting order of Todo issues labelled `legion`, using the Board's rank order.
Click a column heading to sort; hold Shift to add another sort column.
Use **Group by** for status, priority, epic, assignee, created by, claimed by, or an age band.
An epic is an open root issue labelled `epic`.
It is an ordinary row of the table — searchable, filterable, selectable, editable — except while you group by epic, where it is its group's linked heading rather than a row inside it.

The table scrolls vertically and horizontally inside its own box.
Its header stays visible. On phones Select, Key and Title stay pinned within 85% of the box, so a title reads as words; Priority and the other columns scroll sideways.
Title narrows and keeps its two-line clamp, even with saved desktop widths.
The box has room for at least three full issue rows; on compact windows, scroll the page to reach the table and its pager.
Drag a header edge to resize a column, double-click it to reset its width, or drag a header to reorder columns.
**Columns** hides or shows optional columns; widths, order and visibility are remembered per project.
Choose 25, 50, 100, 250 or All rows per page.
A page starting inside a group repeats that group's heading.

The horizontally scrollable status strip above the table shows full lifecycle labels and counts; every segment is a touch-sized button. Click a segment to filter to that status.
It counts what is loaded, so with closed issues unloaded it shows no Done segment at all.
Open issues load first; Done loads when the Status filter includes it or is cleared.
The open rows and filters stay usable while Done loads, or if that read fails; **Retry** asks again.
Until Done has loaded, its filter option says so instead of claiming there are no closed issues.
**Clear filters** also clears Needs you and Unread.
Tick rows to open a floating action bar: **Start next**, **Move to epic…**, **Take out of epic**, or **New epic from these**.
Press `x` to select or deselect the row your keys are on; the search bar then acts on what you have selected, and leaving the table clears it.
An issue the table stops listing drops out of the selection, so a row listed again comes back unselected.
On phones the bar stays in one horizontally scrollable row above the project content, so it cannot cover the table or pager.
**Select all N matching** includes rows beyond the current page.
Start next moves queued selections ahead of the rest of Legion's waiting line, keeping their queue order.
Each rank change waits for the previous one. A refusal stops the later changes, and the notice identifies every issue that did not move up.
Selecting every queued row says they already start next rather than doing nothing.
An epic group's **Epic actions** can rename it, merge its open work into another epic and close it with a reason, or start all its queued issues next, including those hidden by filters.
Merge moves open direct children and open descendants reached through only closed intermediates. It posts the reason and closes the source only after every move succeeds; otherwise the source stays open and the notice identifies the failed moves.
Closed issues keep their original parent and history.
Move, Take out and New epic skip selected closed issues and report how many were skipped; New epic requires at least one open selection.
Naming and renaming an epic happen in a dialog in the page, like creating an issue.
If a new epic may duplicate another issue, **Create anyway** is an explicit choice.
If Dispatch created the epic but lost its answer, the table says so and leaves the selected issues in place rather than guessing the new epic's key.
Independent bulk changes register together and finish independently, at most eight requests at a time. Each refused request stays in the shell's pending-writes indicator and has its own **Retry**; it does not hold the others.
Reload restores requests already registered. New epic registers its parent changes only after the create answers; merge registers its reason and close only after its moves succeed.

Changes made in another tab or by another person appear live without reloading.
Selection and the row being read stay in place when a live update inserts rows above it.
The Board keeps its own rank order within each status; sorting the table does not reorder the Board.

![The Board, with a column per status and cards showing priority, labels, and open asks](/legion/media/dispatch/board.png)

### Moving cards

The Board has a column per status. Drag a card to reorder it or to change its status:

- With a mouse, press and move the card.
- On a touch screen, hold the card briefly, then drag. A quick tap opens the issue, and a swipe
  scrolls.
- With the keyboard, focus a card and press `Shift+J` or `Shift+K` to move it down or up, and
  `Shift+L` or `Shift+H` to move it to the next or previous status.

Dropping a card in Done closes the issue. Moving it out of Done reopens it.

Icebox and Done start as narrow folded columns. They still take drops. **Show Icebox & Done**
opens them in full, and Dispatch remembers that choice.

If someone else moved cards while you were dragging, the move fails with "The board changed while
you were moving this card - refreshed, try again." The Board shows the fresh order.

### Lanes

**Show lanes**, beside **Show Icebox & Done**, splits the Board into horizontal bands, one per
priority: P0 down to P3, then a band for issues with no priority. Cards keep their rank order
within each band. The choice persists per signed-in user, not in the page address, so it survives
a reload.

Dragging a card across lanes sets its priority, in the same drop that a drag across columns sets
its status. Keyboard roving (`j`/`k`, the arrow keys) stays within the focused lane.

### Priority

Priority runs from P0, the highest, to P3. An issue with no priority shows a muted **Priority**
tag. The badge is a picker wherever it appears: the issue header, a Board card, a List row, and an
Inbox row. Choose it and pick a value. Closed issues cannot change priority.

### Assignee

The assignee is the person every ask on the issue waits on until somebody hands that ask to
another person, and whose Inbox lists those asks.
Change it from the issue header. The choices are **Unassigned** and every login allowed to sign in.
An agent that creates an issue without naming one gets the person its token acts for, else the
parent's assignee, else the project's default assignee: the person you name in the projects table
in **Settings → Projects**, who takes every issue the chain leaves without an owner. Only a person
sets a project's default; no agent command does.

### Labels

Choose **Labels** in the issue header to edit an issue's labels. The list offers every label used
in the project, and you can create a new one from the search box. Your changes save when the list
closes.

### Filters

In the List, each column's filter button opens a searchable list of values and their matching counts.
Title instead takes a typed, case-insensitive text filter.
Values selected in one column match any of them, except Labels, which uses all-of matching: an issue must carry every picked label, compared case-insensitively in both List and Board.
Different columns narrow one another.
Each option count applies the search and every other column's filter, not its own column's picks.
The **Search issues** box matches every word across keys, titles, epics, assignees, creators, claim holders and labels.
A word of five or more characters can be one edit away.

The Board keeps the **Filters** strip: Labels requires every selected label, and Search matches keys and titles.
The List and Board share **Needs you** (open asks) and **Unread** (unseen activity).
The List folds unclaimed work into its **Claimed by** column: its **Nobody or lapsed** value lists the issues nobody holds plus those whose claim the server would hand over. The Board keeps its **Unclaimed** chip, and both read the same `unclaimed=1` address parameter.
The Board's active-filter chips remove individual picks.
Filters, table sort, grouping, search and page live in the address, so a reload or shared link restores them.

An issue an agent is implementing shows **Claimed by** and the agent's name, which opens its
[live view](/legion/dispatch/agents/#the-live-view). The claim holds while that agent's session is
`live`, and while it is `unresponsive` with a last registration less than 20 minutes old: an
unresponsive session has missed its registration window but is still working, and its issue stays
claimed. The chip then reads **unresponsive since** the moment the session was last seen, and
names the moment the issue becomes takeable, 20 minutes after that one. Another agent's take is
refused until then, naming the same moment; you can take it yourself at any time. From that
moment the chip reads **takeable now** in the tone of a free issue, and the List's **Nobody or
lapsed** value (the Board's **Unclaimed** chip) lists the issue, while the session is still drawn
as unresponsive, because it has not ended and
may yet answer. A session that reads `gone` is unclaimed at once: its
[running mark](/legion/dispatch/agents/#how-dispatch-shows-an-agent) is a hollow ring and the chip
reads **not running**.

## Creating an issue

Press `c` anywhere. Pick the project, type a title of up to 1,000 characters, and optionally the
first line of the spec. A short moment after you stop typing the title, Dispatch lists the items
it already holds on the same subject — issues, asks, comments, messages and documents — each a
link that opens in a new tab, so your draft stays as you left it. When it searched and found
nothing it says so, and when it could not search at all it says that instead, with the words to
search for yourself. If the title closely matches an existing issue in the project, Dispatch
shows that issue and offers **Create anyway**. A new issue opens as soon as it is created.

## The issue page

![An issue's spec in the editor, with a decision block in the document and a comment thread in the margin.](/legion/media/dispatch/issue-spec.png)

The issue page draws one issue as one tree. From the top down it holds the header, up to three
sentences that say what the issue is, a row of five presets, the issue's child issues, and the
tree. A preset chooses which part of the tree you see. The panel on the right holds the rest of
the issue's properties and its conversation.

### The header

The header stays at the top of the screen while you scroll. It has four rows:

1. The parent issue as a link, when there is one, then the issue's key, a control that copies
   it, the pin, and **Issue actions**, the `⋯` at the end of the row. **Issue actions** holds
   **Close issue**, or **Reopen into Backlog** on a closed issue, and **Set parent issue**, or
   **Edit parent issue** when the issue has a parent.
2. The title, in full, wrapped at any width. Choose it, or press `e`, to edit it.
3. **Status**, priority, assignee and labels; **Add**; the progress counts; the approval chip
   once someone has asked for the spec to be approved; and whose turn it is: `Waiting on you (N)`
   or `Waiting on agents (N)`, counting the issue's open asks.
4. **Messages go to** and the issue's route; and, while an agent holds the issue, **Claimed by**
   and that agent, with **Release**.

**Add** lists each property the issue has no value for yet: **Parent**, **Assignee**,
**Labels**, **Messages go to**, **Blocked by**, **Repository** and **Components**. Pick one to
open the control that sets it. A closed issue has no **Add**, and shows **This issue is closed.**
above the rows. On a screen narrower than 768 px the fourth row is at the end of the page.

A list that still clamps a title shows the whole of it on hover.

Under the header, up to three sentences say what the issue is. They are the sentences of the
spec's first `callout` block, and an unfinished last sentence is left out. Nothing is drawn there
until the spec has loaded. A spec with no such block reads **This spec has no summary.** A
summary cut inside its first sentence reads **The summary is too long to show here.**, and one
with no sentence end at all reads **The summary has no complete sentence.** Two lines follow:

- **How it gets done** links the **Plan** preset and gives the plan's approval state, such as
  **Awaiting approval**, or says **This issue has no plan tasks yet.**
- **What you will be shown** links the **Checks** preset and names each state its checks are
  in, such as **Passed** or **Failed**, or says **This issue has no checks yet.**

### Changes Dispatch has not confirmed

Every change you make — a status, a priority, an assignee, labels, a pin, an answer to an ask, a
comment, a comment resolved, an upload — is sent under a name of its own, and Dispatch makes it
once under that name however many times the page asks. So a change tried again after a lost
answer, a timeout or an outage lands exactly once, and the page keeps asking while your connection
is down, and again at once when Dispatch is reachable again; while Dispatch keeps answering the
change with an error, or the change keeps failing on its way while the rest of Dispatch answers,
the tries spread out, up to 30 seconds apart; when Dispatch answers that it is too busy and says
how long to leave it, the page waits exactly that, for a change and for what the page reads alike.
A change Dispatch is slow to finish is waited
for and made once, and an upload still going up is never cut off, however long a slow link takes.

A corner of the screen accounts for the ones still out: **Saving 2 changes…**, and once one has
gone 30 seconds unconfirmed, **Dispatch has not confirmed 2 changes — still trying**, with **Stop
trying** beside each. An upload still going up reads **Uploading… 40%** instead, and never still
trying unless a try has failed. A change waiting behind an earlier one that has not gone out yet
reads **Waiting to send**, with **Discard**. A change Dispatch refuses shows its reason where you
made it, with **Retry**, when that place is a form or a field you are in front of: a comment, an
answer, the title, the priority, the assignee, the repository, the components, a new issue. A
quick control such as **Status**, **Labels** or a pin shows your choice while it is out, puts
itself back as it was if it is refused, and the corner lists the change as **Not saved: …** with
why, **Retry** and **Discard**. Changes to one issue go out in the order you made them, a card
moved on the board among them. While any change is unconfirmed the browser asks you to confirm
before you leave or reload the page, and a change a reload interrupted, an upload's file included,
is picked up again when the page comes back, in the order you made them, for the person who made
it and for nobody else: a change pending when someone else signs in at that browser, in this tab
or another, is dropped unsent. If your sign-in lapses, your changes wait, and go out once you sign
in again: the sign-in page lists them under **Kept until you sign in again as …**, a refused one
with **Discard**, and its sign-in link leaves the page without asking. Signing out asks you first,
listing each change it would discard, in this tab and in your other Dispatch tabs (when a tab does
not answer at once, it says it would also discard **any unsent changes in your other Dispatch
tabs**), and discards nothing until Dispatch has signed you out: meanwhile none of your changes
goes out in any tab (another tab's read **Waiting for a sign-out in another tab**), and if the
sign-out fails you stay signed in with every change, told **The sign-out did not go through.**
If that tab closes before it can say how the sign-out ended, the others ask Dispatch who is
signed in: signed out, your changes go there too, as the sign-out said they would; still signed
in, they go out again within seconds.
One interrupted more
than five minutes ago is not sent on its own: the corner lists it as **Not saved: …** with
**Retry** and **Discard**. A Retry of a stopped or interrupted change asks again under its first
name, so it is saved at most once if you retry within 24 hours of the first try; after that
Dispatch no longer knows the name, and the change may be made twice. The corner says so.

A closed issue takes no upload and no new document. In the **Files and documents** menu,
**Upload** is shown as unavailable, with **This issue is closed. Reopen it to add a document.**,
and **New document** is not offered. The list at
`/issues/<KEY>/artifacts` says **This issue is closed. Reopen it to add a document.** beside its
**New document** and **Upload**: it picks no file, no key opens a file picker there, and a file
dropped on it is ignored.

Now and then Dispatch makes a change but its answer is lost on the way back. The change is made;
the page says so (for example **The issue “…” was made, but Dispatch lost its answer, so it may
take a moment to appear.**) and clears the form, so pressing again makes nothing twice. A new agent
token made that way has lost its value with the answer. The page looks for the tokens of that
label made since you pressed **New token** and offers to revoke it and mint another in its place;
if it finds more than one, you pick which, and every revoke asks you to confirm first, since
agents using it stop working at once.

### The tree

The tree holds everything the issue holds, one row per node: its documents, and in them every
note, ask, requirement, plan task and check block; its decisions; the results recorded against
each check; its comments, messages and handoffs; its images; and its child issues. A check and a
plan task sit under each requirement they serve, and a result sits under its check. A node whose
source Dispatch does not hold sits under the issue.

The tree opens folded to its frontier. The path down to each thing that is open and unanswered
is unfolded, however deep it is: an ask nobody has answered, a check that has not passed, a
decision that waits for your review, and a requirement no check serves. Every other branch is one
row that says how many rows it hides, such as **3 rows hidden inside**, and a replaced decision's
branch stays folded. A branch you fold stays folded, and a branch you unfold stays open. From the
third level down, unfolding a branch shows one row, such as **3 more rows below**, which opens the
rest.

A row is one line: what the node is, who wrote it, its state, its links, and what it lacks in a
few words. Who wrote it is a name and a shape: a person by their login, with one shape, and an
agent by its session's title, with another, or **An agent** when the session has no title. A
decision marked unsure says **Unsure**. A row marks what it lacks only where that sets it apart
from rows of its kind and names work someone owes: **No check** on a requirement no check serves,
**No reason given** on a plan task that serves no requirement, **Not a requirement** on a check or
a plan task whose **serves** names something else, and **No proof it can fail** on a check whose
latest result records no negative control. Focus a row, with the pointer or a key, and its body
opens under it with the rest: its facts, and each lack as a sentence. The body closes when the
focus moves on, unless you choose **Keep open**; **Let it close** undoes that. A faint row is
there only because a row under it matches the preset. A decision that waits for your review has
**Keep** and **Change** in its body: **Change** opens **Your change**, with **Send change** and
**Cancel**.

Nothing on the issue page opens when you rest the pointer on it, a link included. A link's
preview card opens when you focus the link with the keyboard, or, in a document you are editing,
when the caret is in the link. Following a link or a row's link to another node of the issue puts
the focus on that node's row, in whichever preset the link opens.

Press `Enter` on a row to edit its text where it stands; `Escape` takes you back to the row. `.`
zooms into the focused row: the tree is drawn from that row down, and the path above it is a line
of names, each of which zooms back to it. `,` zooms back out. At 1024 px and below the tree is
always read one node at a time: it shows the node you are on and the rows under it, with the path
above it as that line of names, and it unfolds the path down to the first open question.
[Keyboard shortcuts](/legion/dispatch/keyboard/#issue-tree) lists every key of the tree.

### The presets

A preset is a view of the one tree. It draws the nodes of the kinds it is for, with the rows
above each one, so a node sits under the same parent in every preset. The count beside a preset
is how many nodes it holds.

| Preset | What it draws | Address |
| --- | --- | --- |
| **Tree** | Every node, with the spec under the tree | `/issues/<KEY>` |
| **Decisions** | Asks and decisions. It opens the body of the first decision that waits for your review | `/issues/<KEY>/decisions` |
| **Checks** | Checks and the results recorded against each one, with the Verification document, when there is one, under the tree | `/issues/<KEY>/verification` |
| **Spec** | Requirements, with the spec under the tree | `/issues/<KEY>/spec` |
| **Plan** | Plan tasks, with the plan document under the tree | `/issues/<KEY>/plan` |

Under the spec, **Requirements** lists the spec's requirements, and marks each one no check serves
**No check**.
[Documents](/legion/dispatch/documents/) covers the spec itself. The Plan preset puts the plan's
approval chip above the plan document, and under it each plan task the document marks, with the
requirements it **serves** and the tasks it **needs**. An issue with no plan document shows there
the `dispatch artifact` command that makes one.

A check reads **Passed**, **Failed**, **Out of date**, **No result**, **Retracted**, **Cannot
tell** or **Could not read**. Its body shows its **when**, **surface**, **run** command and
**negative control**. Each result under it shows its **verdict**, **Pass** or **Fail**, and, where
Dispatch can tell, **how it stands**: **Up to date**, or **Out of date** when its commit is not
one the check compares results with or the check's words changed after it. It also shows its
**role**, its **commit**, and what it counted.

Choose a preset to open it. While a preset has the focus, `←` and `→` move to the previous and
next one, and `Home` and `End` to the first and last. At 1024 px and below the row is one control
that names the preset you are on and opens a menu of the five.

Beside the presets:

- **Files and documents** lists each document and file of the issue that no block cites, marked
  **Cited by nothing**, each with a copy control. A document a block cites is a row under that
  block instead. The menu also holds **Upload**, which uploads the file you pick at once, and
  **New document**, which asks for a title.
- The **Filter** box narrows the preset to the rows whose text holds what you type, and the rows
  above them, with every branch it keeps unfolded. The counts follow it, and the address keeps it
  as `?q=`.
- The toolbar of the document the preset has open: its **Version**, **Name**, connection dot,
  copy control, **Copy link** and approval chip.
  [Documents](/legion/dispatch/documents/#versions) covers them. On a screen narrower than
  768 px the toolbar is under the tree.

### The panel

On a wide screen the panel is in the margin, under the margin's own comment cards. Where the
margin is a sheet, or you hid it with `Shift+M`, the panel is under the header. Moving between
the two keeps what you typed in it.

Three sections hold the rest of the issue's properties. Choose a section's name to close or open
it. This browser keeps your choice for the next issue you open.

- **Development**: **Repository**; **Components**, the parts of the project's architecture the
  issue belongs to, which you can change when the project has an architecture source, picking
  from the components of every source; **Blocked by**, the issues this one waits on; and
  **Links**, the GitHub issues and pull requests linked to it.
- **People**: **Opened by**; **Subscribers: N**, which lists the agent sessions subscribed to the
  issue, where **Unsubscribe** stops delivery to one and tells it so; and **Collaborators**,
  which lists everyone who has worked the issue. That list has Legion's agents by role, each
  role's newest session first and a replaced session naming the one that replaced it; then every
  other agent session, the most recently active first; then the people, with why each is a
  collaborator and when they last worked it. Every agent is one click from its conversation and
  shows whether it is running. When Legion's record of this issue's agents cannot be read, the
  list says so instead of listing nothing. An issue nobody has worked reads **No one has worked
  this issue yet.**
- **References**: **Referenced by (N)** lists every issue, document, ask, comment, and message
  that links here.

**The row you are on** gives the focused row's kind and text, its **Status**, and **Written by**
with its author. With no row focused, it gives the issue's.

Under them is the **Conversation**, with four choices:

- **Everything**: every conversation. At `/issues/<KEY>/activity` it also draws the issue's
  whole history under the tree.
- **Pinned**: only the conversations you pinned.
- **Questions**: only the asks.
- **What people said**: every conversation.

This browser keeps your choice for the next issue you open, and until you choose it is **What
people said**. Some addresses choose for you: `/issues/<KEY>/conversation` and the address of one
ask, comment or message choose **What people said**, and `/issues/<KEY>/activity` chooses
**Everything**. The conversation's list appears under the four once you choose one of them, or
open `/issues/<KEY>/conversation` or the address of one ask, comment or message.

The list has one card per conversation: every ask with its replies and its answer, every comment
thread with its replies, every message thread with its replies. The conversation with the newest
reply is at the top, so a thread rises when someone answers it, and the whole list is there
however old a conversation is. **All**, **Open** and **Closed** filter it: an ask is Closed once
it is answered, resolved or retracted, a comment thread once it is resolved or its suggestion is
accepted or rejected, and a message thread is never closed. The filter opens on **All** every
visit. A conversation holding something you have not read is marked **New**, and **Jump to
latest** counts those and takes you to the top.

The issue's whole history is at `/issues/<KEY>/activity`, under the tree while **Everything** is
chosen. It has one line each, newest first: every comment, ask and message as it was posted, and
everything else that happened to the issue — status moves, claims, document versions, uploads,
subscriptions, changes to child issues. It has **Load older** at the bottom, a divider for each
day, and **New since you last read** where your last read ends.

### Child issues

An issue with child issues has a **Children** section above the tree, on every preset. Its line
has a progress bar and two counts: **Children done**, for the issue's own children, and **Whole
tree done**, for every issue under it. Open, it lists each child on one line: its key and title,
its status, **Blocked by** and the issues it waits on, **Claimed by** and the agent that holds
it, its latest activity, and its GitHub links. This browser keeps whether the section is open for
the next issue you open.

### Addresses

Each of these addresses opens the issue page, and links made before the page had presets still
open:

| Address | What it opens |
| --- | --- |
| `/issues/<KEY>` | The Tree preset, with the spec under the tree |
| `/issues/<KEY>/spec` | The Spec preset, with the spec under the tree |
| `/issues/<KEY>/verification` | The Checks preset, with the Verification document under the tree |
| `/issues/<KEY>/plan` | The Plan preset, with the plan document under the tree |
| `/issues/<KEY>/decisions` | The Decisions preset |
| `/issues/<KEY>/conversation` | The Tree preset, with **What people said** chosen and the conversation's list in the panel |
| `/issues/<KEY>/activity` | The Tree preset, with **Everything** chosen and the issue's whole history under the tree |
| `/issues/<KEY>/children` | The Tree preset. The **Children** section is above the tree on every preset |
| `/issues/<KEY>/artifacts` | The Tree preset, with **Files and documents** open and every document and file of the issue listed under the tree |
| `/issues/<KEY>/collaborators` | The Tree preset, with **Collaborators** open in the panel's **People** section |
| `/issues/<KEY>/artifacts/<name>` | The Tree preset, with that document or file, its versions and its details under the tree, and the tree unfolded to its row. The spec's own name opens the Spec preset |
| `/issues/<KEY>/asks/<id>`, `/comments/<id>` or `/messages/<id>` | The Tree preset, with **What people said** chosen, the panel's conversation on that entry, and the tree unfolded to its row |

On any of them, `?q=` fills the **Filter** box, and `#b-` with a block's id, `?ask=` or
`?comment=` unfolds the tree down to that node. `?v=` with a version number reads that version
of the spec on `/issues/<KEY>/spec`, and of the document on `/issues/<KEY>/artifacts/<name>`.

### Writing in the Conversation

The box at the top of the Conversation's list, in the panel, posts a comment on the issue. Below
640 px it sits at the foot of the screen. It works on a closed issue too: there the composers,
**Reply** on a message and a delivery's **Retry** all work, and you can still comment on a quote
of its documents: select the text and choose **Comment**, the one action the selection bar offers
on a closed issue's document, and the comment opens in the margin. Type `@` to mention an agent
role or a live agent session; the comment is then delivered to it. The picker
also offers, under **Not running**, this issue's Legion roles that no running agent holds; a
mention picked there is addressed to the role, and the comment is posted on the issue, but its
delivery fails while nobody holds that role, since there is no session to take it. The thread
shows that failed attempt with **Retry** beside it, which sends to whoever holds the role at the
time; that Retry is offered for three days, after which a fresh comment is the way to reach the
role. Start the comment with `/btw ` or `/aside ` to choose how a mentioned agent receives it.
[Agents](/legion/dispatch/agents/#sending-to-one-agent) explains the delivery modes.

Press `Ctrl+Enter` (`⌘Enter` on a Mac) to send. `Enter` alone starts a new line.

## Pins

Pins keep things you return to within reach.

- **An issue.** Choose the pin in the header's first row, or press `Shift+P`. Pinned issues
  appear under **Pinned** in the sidebar, with their open-ask counts.
- **A Conversation entry.** Every entry has a pin at its top right. Pinned entries appear in the
  margin's **Pinned** tab beside the document, each with **Unpin**, and **Pinned** in the panel's
  Conversation lists only them.
- **An agent.** On the Agents page, a pinned agent stays at the top of the list.

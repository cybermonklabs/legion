/** Why each spec file's remaining rows stay on the real stack, read by `e2e/tools/row-map.ts`.
 *
 *  dispatch://INFRA-1407's k2 asks, for every row the change did not move, the server behaviour
 *  its assertion depends on. The reasons are here rather than as a comment in each of the
 *  hundred and forty spec files that keep rows, because a comment in each would touch every
 *  file of a suite several trees edit at once and conflict with all of them; gathering them
 *  makes the list readable in one pass as well. The generation refuses a file the listing names
 *  that is absent here, so the map stays complete and machine-checked either way.
 *
 *  A reason names what the server decides for that file's rows: the four shapes
 *  `packages/dispatch/AGENTS.md`, "The stubbed layer", sets out. Where a file's rows stay for
 *  more than one, the reasons are joined with a semicolon.
 *
 *  A file whose every row moved is absent from this list, and so is a file no project of this
 *  listing runs - the multiprocess specs among them, which the generation never names. It
 *  refuses a file it lists that has no reason here, and a reason here for a file it does not
 *  list, so this list is exactly the suite's real-stack files and nothing else. */
export const realStackReasons: Record<string, string> = {
  "accessibility.e2e.ts":
    "three of its scanned routes wait on the live document's connected status, which the Yjs socket alone gives",
  "activity.e2e.ts":
    "pins/unpins and asserts the refetched margin; pins first, then asserts on the server's stored body",
  "address-links.e2e.ts": "asserts in the live spec editor (`/ws/doc`) as well as the Conversation",
  "agent-context-ring.e2e.ts":
    "the ring is drawn from the context frames a session publishes, which the live relay carries, and a frame published after load must reach the open page",
  "agent-list-refilling.e2e.ts":
    "drives the fake Envoy listener's refusal states and asserts the page's reading of them",
  "agent-live-layout.e2e.ts":
    "the header's pin is written to Dispatch and read back from its stored state, and every row opens on a view the relay builds from the session's replay",
  "agent-live-views.e2e.ts":
    "a view that sleeps and comes back is answered with the session's replay, which the live relay alone gives; a reply seeded through Dispatch after load changes the unread count the rail reads, and the view in front writes the read mark that clears it",
  "agent-name-band.e2e.ts":
    "writes the session's two replies, one carrying a title and one carrying none, and then reads the name the server resolves from them (`delivery.ResolveSessionTitles`); the fake registry must drop the session before the page loads, or the roster names it and the read proves nothing",
  "agent-name.e2e.ts":
    "margin comment on the live document, plus the fake registry; lands on the live view, whose frames come from the relay; a registry change after load must reach the open page",
  "agent-rail.e2e.ts":
    "a pin is the viewer's per-agent state written to the server and read back - from the rail row, the Agents row and the live view header, in a second browser context that reloaded nothing, and out of this browser's own old list into what Dispatch stores",
  "agent-shell-command.e2e.ts":
    "the view is built from the session's replay over the relay, and each turn is drawn under the id Dispatch minted for the message it answers, folded with the reply the server stored",
  "agent-skill-completion.e2e.ts":
    "completion reads the session's commands from the replay the live relay carries, and what Send hands over is read back from the fake Envoy",
  "agent-tokens.e2e.ts": "token mint, attribution, privacy between two contexts, revoke",
  "agent-view-cost.e2e.ts":
    "every view opens on a transcript at the stream's limit the relay replays, and the gap row turns on the relay itself: a frame published while a viewer is subscribed reaches the page, and one published to a sleeping view reaches nobody and is kept nowhere",
  "agent-view-settings.e2e.ts":
    "live session settings frames, switch round trips through fake Envoy, `page.reload()` after writes",
  "agent-view.e2e.ts": "every row replays or streams session frames over the live relay",
  "agent-workspace-timing.e2e.ts":
    "times the page against six hundred sessions of the fake registry and a limit-sized transcript the relay replays, both of which a recording would freeze",
  "agents-groups.e2e.ts":
    "the page groups on what the server answers: the asks it holds open for the reader, the event it stamps a session's activity with, and last_seen against the wall clock, which a recording freezes; a row's pin is written and read back in the rail",
  "agents-search.e2e.ts":
    "every row reads the Agents list, which folds a session unseen for ten minutes against the clock, and a recording freezes last_seen",
  "agents-window.e2e.ts":
    "six hundred sessions come from the fake registry, and the one group the page counts all six hundred under, Idle, is their last_seen against the wall clock, which a recording freezes",
  "agents.e2e.ts":
    "sends a BTW message and asserts the exchange; Clear is a write re-read after a reload; needs a session reply delivered live",
  "anchors.e2e.ts": "live document edits with the server's anchor state read back",
  "answers.e2e.ts": "answers and answer changes written, then re-read on other surfaces",
  "approval.e2e.ts":
    "approval currency and staleness are the server's computation over versions, with writes and reloads",
  "architecture-sources.e2e.ts": "fake GitHub import, saves and a reload",
  "architecture.e2e.ts":
    "a status write must move the server-computed bar; the server's import refusal is the assertion",
  "artifacts.e2e.ts": "uploads, version compare, document sockets and a 503 refetch",
  "ask-blocks.e2e.ts":
    "decision answers the server records, and pastes in the live document editor",
  "ask-concurrent-edit.e2e.ts": "two editors crossing in one `/ws/doc` room",
  "ask-part-references.e2e.ts": "the reference and backlink index the server builds from the write",
  "asked-of-cli.e2e.ts":
    "runs the real `dispatch open-asks` against the server and reads the person it names, which the server's own expression answers, before and after a hand-on made in a browser",
  "asked-of.e2e.ts":
    "hands an ask on and re-reads the write on every surface that draws it; whose Inbox the row sits in is the server's own expression, and each move arrives over the live socket with no reload",
  "attention.e2e.ts":
    "the new card arrives over the live socket; a read-mark write moves under the reader",
  "bare-references.e2e.ts": "opens the spec's live editor and its margin for every assertion",
  "block-references.e2e.ts":
    "uploads, then asserts the server re-resolved the reference; an edit after load must arrive live; a delete after load must arrive live",
  "board.e2e.ts":
    "drag writes a rank, read back after a reload; rank writes; the server's rank rejection is the assertion",
  "broadcast.e2e.ts": "broadcast sends, server-side exclusion, retry and idempotency",
  "broker-approval.e2e.ts":
    "decisions made on the real broker, with sign-in and cross-origin refusals",
  "btw.e2e.ts": "delivery modes and sends asserted against the fake Envoy",
  "claim-refilling.e2e.ts": "the claim's lapse is the server's judgement against the agent list",
  "claim-unresponsive.e2e.ts":
    "unresponsive, takeable and a second agent's refusal are server decisions",
  "claim.e2e.ts":
    "a claim is taken, refused and released by the server, and the page reads back what it decided",
  "close-withdraws-asks.e2e.ts":
    "close withdraws asks server-side, then reopen and version history are re-read",
  "closed-issue-posting.e2e.ts": "posts on a closed issue and asserts the server accepted them",
  "code-line-replace.e2e.ts": "typing in the live document editor",
  "collab-cursor.e2e.ts": "collaborator cursors over `/ws/doc`",
  "collaborators.e2e.ts": "Legion's record of the issue and a role mention delivered",
  "compose-wait.e2e.ts": "anchors settled by the server, live document edits by a second writer",
  "composer-body-cap.e2e.ts":
    "the server's refusal or acceptance of a body length is the assertion",
  "conversation.e2e.ts":
    "names resolve from the live registry; an edit after load arrives live; the prefilled mention is sent and checked",
  "coverage.e2e.ts": "coverage flags are the server's computation, cleared by a write",
  "create-document-refusals.e2e.ts":
    "every row reads a refusal: the taken title the control checks against the owner's own list, the role slug and the duplicate the server refuses in its own words, and the closed issue's one sentence; each then reads the issue's documents again to prove nothing was written",
  "create-document.e2e.ts":
    "makes a document and then reads its own write - the versions it holds, the list it is listed in and the line typed into its live editor after a reload; the double press is answered by the server's idempotency key",
  "create-issue-decision-claim.e2e.ts":
    "the server's search answers the typed title, and the answered ask it lists is its ranking",
  "create-issue-related.e2e.ts": "the server's similar-issue search answers the typed title",
  "decision-records.e2e.ts":
    'Keep and Change are writes, and "out of date" is the server\'s judgement',
  "deep-links.e2e.ts": "document landings need the live room and server-settled anchored marks",
  "delivery-groups.e2e.ts": "grouping and the window's counts are the server's aggregation",
  "delivery-live.e2e.ts": "`sql` writes delivered as live frames",
  "delivery.e2e.ts":
    "facet counts come from the server; a reconcile error must arrive from the server mid-gesture; the title says it: the server narrows",
  "doc-reload-line.e2e.ts": "the read-only admission is decided at socket open",
  "doc.e2e.ts": "every row lives in the live document room, its connections and versions",
  "document-and-comment-read-alike.e2e.ts": "each row asserts inside the live spec editor",
  "document-way-back.e2e.ts":
    "a correction and two removals written and then re-read on the open page, the issue's list and its Activity; the refused removal is the server's judgement that the document holds writing, which only a settled version makes true",
  "document.e2e.ts": "collaboration, uploads, marks, and the live connection dot",
  "editor-paste.e2e.ts": "pastes into the live editor, read back after a reload",
  "email-links.e2e.ts": "asserts in the live editor before the past version",
  "failed-room.e2e.ts": "a document room that fails and recovers",
  "followers.e2e.ts": "follower and subscriber writes change what the server delivers",
  "gated-step-approvals.e2e.ts":
    "gate state is the server's computation over versions; server-computed gate state; the write answers both steps server-side",
  "github-checks.e2e.ts": "the checks pill is the server's evaluation of fake GitHub runs",
  "going-bar.e2e.ts":
    "every row types into the bar and reads what the index and the recent-items list answer, and one writes in the live document editor",
  "going-handoff.e2e.ts":
    "the hand-off row is offered only while a live session holds the issue's claim, and approving it is a message the fake Envoy records",
  "going-key-forms.e2e.ts":
    "each form of a key is resolved by the server, and the row reads the rows it answers",
  "going-new-issue.e2e.ts":
    "the bar creates an issue and the row re-reads what the server stored, its messages and its results",
  "going-operators.e2e.ts": "each operator form and each chip narrows a list the server answers",
  "handoffs.e2e.ts": "needs the live frame",
  "heavy-bodies.e2e.ts":
    "its claim is a wall-clock time, so it runs one row at a time in the -alone projects and in no other",
  "heavy-streamed-turn.e2e.ts": "a long turn streaming over the live relay",
  "held-sends.e2e.ts": "sends refused by the server and held across pages",
  "hover-preview.e2e.ts":
    "the row it keeps is a phone gesture read in the iphone configuration, which no stub project repeats",
  "inbox-approval-message.e2e.ts":
    "the reading pane draws the document under review from its live room",
  "inbox-cli.e2e.ts": "runs `dispatch inbox` against the server and compares it with the page",
  "inbox-decline.e2e.ts":
    "every decline is a write the server takes and the surfaces re-read; the kind it refuses to decline is the server's; the note's bound is the route's",
  "inbox-dismiss.e2e.ts": "dismiss and restore writes, bulk requests, refusals and reloads",
  "inbox-drawer.e2e.ts": "answers an ask in the drawer",
  "inbox-mark-all.e2e.ts": "mark-all is a server write, including its refusal",
  "inbox-pane-scroll.e2e.ts":
    "every row draws the document in its live room; another session's edit, a reply and an event committed elsewhere must reach the open page, and a resumed socket's gap must refresh the reads it holds; an approval row reads the version the server names latest against the approval it is marked against",
  "inbox-read-state.e2e.ts": "reading writes the read mark and the row is re-read",
  "inbox-reading-pane.e2e.ts":
    "the answer is a write; approval is the server's computation; the withdrawal is the server's",
  "inbox-stability.e2e.ts":
    "rows move because writes land elsewhere and arrive live under the reader's hand",
  "inbox-thread-pane.e2e.ts":
    "opens the document's live room; the reply is delivered and re-read; the session's turn comes from the relay",
  "inbox-threads.e2e.ts":
    "the drawer's own bar is read after a navigation cancels the first agents read, which a recording answers with the cancelled entry",
  "inbox-view.e2e.ts": "a priority write must arrive live",
  "inbox.e2e.ts":
    "writes a reply and re-reads the thread; live event feed; lands in the document's live room",
  "issues-table.e2e.ts":
    "every row writes and then reads its own write: parents, ranks, epics and merges patched through the API and read back through `getIssue`, plus a refused write retried from the store and an interrupted action finished after a reload",
  "issue.e2e.ts":
    "asserts the fake Envoy's state across resets; status writes re-read; writes persisted and re-read",
  "judged-grant.e2e.ts":
    "the two grants it reads are written into the fake broker and the page is asserted against what the server lists back",
  "keyboard-agents-picker.e2e.ts":
    "each asserts where a send, reply, retry or upload was addressed, and the server's refusals",
  "keyboard-agents-pin.e2e.ts": "sends, uploads and read marks in flight across the pin",
  "keyboard-agents.e2e.ts":
    "the upload's target is the server's record; upload and send against the server; the delivered mention is the assertion",
  "keyboard-document.e2e.ts": "comments made from the live document editor",
  "keyboard-inbox.e2e.ts": "snooze write; server refusal; server refusals",
  "keyboard-issue.e2e.ts":
    "writes a priority and a pin, then reloads; the server's refusal; asserts what was written",
  "keyboard-palette.e2e.ts":
    "the action is a write; the server's search answer; the server's search hits",
  "keyboard-session-pickers.e2e.ts": "the switch is sent to the live session and its answer read",
  "keyboard.e2e.ts": "creates an issue and opens what the API returned; status and rank writes",
  "keyed-send.e2e.ts": "idempotency keys and one delivery proved against the server",
  "keyed-writes.e2e.ts": "idempotency keys, 409 KEY_IN_FLIGHT, reload resumption, sign-out races",
  "labels-escape-focus.e2e.ts":
    "the close commits a label write and the row waits for the server's answer to reach the header before it reads focus again",
  "labels.e2e.ts":
    "label writes and a server-filtered list; writes labels, then reloads and measures",
  "legion-facts.e2e.ts":
    "the facts consumer recomputes one issue's stored row from an event the row writes, and the server's `issue_facts` change row wakes the open Legion view over `/ws/live`; the card's work time is the number the server stored",
  "legion.e2e.ts":
    "every metric and every column is the server's aggregation over the daemon's recorded observations; the settings rows write a setting and read the daemon's own verdict back; a card moves between columns on a live `/ws/live` frame with no reload",
  "live-socket.e2e.ts": "the `/ws/live` socket's own behaviour",
  "live.e2e.ts": "live delivery of writes made after the page loaded",
  "machine-login.e2e.ts": "device grants decided on the real broker",
  "machines.e2e.ts":
    "the draw and drawing-age rows read the wall clock against last_seen, which a recording freezes",
  "margin-composer-hold.e2e.ts": "live document margin with sends out and server refusals",
  "margin-kind-switch.e2e.ts": "two readers in one live document room",
  "margin-order.e2e.ts": "the margin sits on the live document room",
  "margin-thread-reply.e2e.ts": "margin replies in flight with server refusals",
  "margin.e2e.ts": "two contexts, live document marks, suggestion accepts",
  "mention-composer.e2e.ts": "delivery through the fake Envoy and the listener's own refusals",
  "mermaid.e2e.ts":
    "the edit lands and is re-read; each draws the diagram inside the live document editor",
  "message-agent.e2e.ts": "mention targets posted and their delivery failures",
  "nav.e2e.ts":
    "the sidebar's badges are counts the server computes over the issues and asks the row seeds, and the row asserts the exact set of requests the page makes of that server",
  "nested-directive.e2e.ts":
    "its claim is a wall-clock time, so it runs one row at a time in chromium-alone and in no other",
  "offline-edits.e2e.ts": "document transport, IndexedDB recovery and read-only admissions",
  "overlapping-marks.e2e.ts": "two readers' marks and the server's anchor refresh",
  "parse-count.e2e.ts":
    "every row counts what the browser parsed while the page drew a text the live server stored, which is the whole claim: a recording would answer the trees a recording holds rather than the trees this server sends, and its first row seeds the texts the rest of the file counts over, which only a live server does",
  "paste-recovery.e2e.ts": "pastes into the live document",
  "phone-conversation.e2e.ts":
    "sends held, refused and retried, and read marks written from a thread",
  "phone-landing.e2e.ts": "each row holds a conversation read open and continues it to a server",
  "phone.e2e.ts":
    "the trap is asserted while a send is out; asserts the project document's live editor content",
  "picture-link-click.e2e.ts": "the link is clicked inside the live document editor",
  "pictures.e2e.ts": "pastes and uploads, with the live editor and stored markdown read back",
  "pipeline.e2e.ts": "`sql`-seeded pipeline state the server aggregates",
  "plain-http-origin.e2e.ts": "types into the live document over the plain-HTTP origin",
  "plain-http-proxy.e2e.ts": "counts the live socket upstream of a real proxy",
  "popover-escape.e2e.ts":
    "the command bar's own search reads `/api/v1/search`, and the inbox peek lists the asks the server holds for the reader",
  "polish.e2e.ts": "opens the margin ask composer on the live document",
  "prefix-links.e2e.ts": "`sql`-forged id twins and the server's short-id resolution",
  "progress.e2e.ts":
    "it asserts the progress the server computes over an issue's children before it opens a page",
  "project-default-assignee.e2e.ts":
    "which person a question reaches is the server's own expression over three tables, and each of the two writes that move it must reach the open Inboxes, issue page and document page over the live socket with no reload",
  "project.e2e.ts": "the reference closure is the server's graph walk",
  "recipients-claim.e2e.ts": "claims and routes the server computes, with live card updates",
  "reference-cards.e2e.ts": "opens the spec's live editor and its margin thread",
  "reference-titles.e2e.ts": "asserts drawn titles inside the live editor",
  "references.e2e.ts": "the backlink index the server rebuilds after each write",
  "refetch-budget.e2e.ts": "writes after load, counted reads and the server's event feed",
  "refresh-freshness.e2e.ts": "writes after load must reach the open page within 2 s",
  "reset-unnamed-table.e2e.ts":
    "its two rows read what the reset between two rows of this suite does to a table nothing names, in the stack's own database, which no recording holds",
  "result-commit.e2e.ts": "the result line is read in the document margin's Pinned tab",
  "search-tiers.e2e.ts":
    "its claim is the number of embeddings the server asks the fake embedder for, counted on the server's own calls",
  "search.e2e.ts": "the server's search hits; the server's answer for the query",
  "settings.e2e.ts": "mappings, defaults and architecture sources saved, refused and re-read",
  "shell.e2e.ts": "the server must revoke the session",
  "signins.e2e.ts": "the broker's machine list and a Revoke decided there",
  "smoke.e2e.ts":
    "the reader nobody signed in, where the stubbed layer's page is always signed in because its recording answers /auth/whoami",
  "spec-suggestion-decoration.e2e.ts": "reads the decoration's DOM inside the live editor",
  "stored-text-reading.e2e.ts":
    "each row reads a body the live server stored and drew as its own tree, and the server's reading is what the row compares against the browser's: a recording would answer a tree recorded elsewhere rather than the one this server sends for those bytes",
  "table-controls.e2e.ts":
    "the Agents page folds a session by its last_seen against the clock and the Delivery page reads a window of dates, both of which a recording freezes; the rest count seeded rows against each other - a filter's count beside each of its values, the number waiting on the reader, the pager's total - and write before they read: a column resize, a reorder and a visibility kept for the reader, a Board disclosure and an Artifacts narrowing read back after a reload, and Mark all read",
  "task-items.e2e.ts": "asserts inside the live document editor",
  "text-typed-blocks.e2e.ts": "the live document editor, including typing a new option",
  "threads-order.e2e.ts":
    "two live documents, each with its own room; the margin sits on the live document",
  "threads.e2e.ts":
    "replies, resolve and reopen, edits, retractions and suggestion accepts, all server writes",
  "typed-block-content-rule.e2e.ts": "pastes into the live document editor",
  "typed-blocks.e2e.ts": "asserts inside the live project-document editor",
  "verification.e2e.ts":
    "opens the document's live room; where a result's commit stands is the server's comparison with the PR head",
  "visual.e2e.ts":
    "it compares a page against a committed baseline inside the pinned Playwright image, which is its own pair of projects",
  "workspace.e2e.ts": "it asserts the counts and the titles the server groups a workspace into",
  "writer-spelling.e2e.ts": "asserts the readings inside the live document editor",
  "writes.e2e.ts":
    "a failed pin's retry, one comment from a double Submit, and a pin removed from Pinned",
};

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
 *  A file whose every row moved is absent from this list and belongs in no part of it. */
export const realStackReasons: Record<string, string> = {
  "accessibility.e2e.ts":
    "three of its scanned routes wait on the live document's connected status, which the Yjs socket alone gives",
  "activity.e2e.ts":
    "pins/unpins and asserts the refetched margin; pins first, then asserts on the server's stored body",
  "address-links.e2e.ts": "asserts in the live spec editor (`/ws/doc`) as well as the Conversation",
  "agent-list-refilling.e2e.ts":
    "drives the fake Envoy listener's refusal states and asserts the page's reading of them",
  "agent-name.e2e.ts":
    "margin comment on the live document, plus the fake registry; lands on the live view, whose frames come from the relay; a registry change after load must reach the open page",
  "agent-tokens.e2e.ts": "token mint, attribution, privacy between two contexts, revoke",
  "agent-view-settings.e2e.ts":
    "live session settings frames, switch round trips through fake Envoy, `page.reload()` after writes",
  "agent-view.e2e.ts": "every row replays or streams session frames over the live relay",
  "agents-search.e2e.ts":
    "every row reads the Agents list, which folds a session unseen for ten minutes against the clock, and a recording freezes last_seen",
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
  "coedit.multiprocess.e2e.ts": "two processes sharing one `/ws/doc` room",
  "collab-cursor.e2e.ts": "collaborator cursors over `/ws/doc`",
  "collaborators.e2e.ts": "Legion's record of the issue and a role mention delivered",
  "compose-wait.e2e.ts": "anchors settled by the server, live document edits by a second writer",
  "composer-body-cap.e2e.ts":
    "the server's refusal or acceptance of a body length is the assertion",
  "conversation.e2e.ts":
    "names resolve from the live registry; an edit after load arrives live; the prefilled mention is sent and checked",
  "coverage.e2e.ts": "coverage flags are the server's computation, cleared by a write",
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
  "document.e2e.ts": "collaboration, uploads, marks, and the live connection dot",
  "editor-paste.e2e.ts": "pastes into the live editor, read back after a reload",
  "email-links.e2e.ts": "asserts in the live editor before the past version",
  "failed-room.e2e.ts": "a document room that fails and recovers",
  "followers.e2e.ts": "follower and subscriber writes change what the server delivers",
  "gated-step-approvals.e2e.ts":
    "gate state is the server's computation over versions; server-computed gate state; the write answers both steps server-side",
  "github-checks.e2e.ts": "the checks pill is the server's evaluation of fake GitHub runs",
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
  "inbox-dismiss.e2e.ts": "dismiss and restore writes, bulk requests, refusals and reloads",
  "inbox-drawer.e2e.ts": "answers an ask in the drawer",
  "inbox-mark-all.e2e.ts": "mark-all is a server write, including its refusal",
  "inbox-read-state.e2e.ts": "reading writes the read mark and the row is re-read",
  "inbox-reading-pane.e2e.ts":
    "the answer is a write; approval is the server's computation; the withdrawal is the server's",
  "inbox-search.e2e.ts": "a write in flight",
  "inbox-stability.e2e.ts":
    "rows move because writes land elsewhere and arrive live under the reader's hand",
  "inbox-thread-pane.e2e.ts":
    "opens the document's live room; the reply is delivered and re-read; the session's turn comes from the relay",
  "inbox-threads.e2e.ts":
    "the drawer's own bar is read after a navigation cancels the first agents read, which a recording answers with the cancelled entry",
  "inbox-view.e2e.ts": "a priority write must arrive live",
  "inbox.e2e.ts":
    "writes a reply and re-reads the thread; live event feed; lands in the document's live room",
  "issue.e2e.ts":
    "asserts the fake Envoy's state across resets; status writes re-read; writes persisted and re-read",
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
  "labels.e2e.ts":
    "label writes and a server-filtered list; writes labels, then reloads and measures",
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
    "the sidebar row is read in the desktop and the phone configuration at once, and a stubbed row runs in exactly one",
  "nested-directive.e2e.ts":
    "its claim is a wall-clock time, so it runs one row at a time in chromium-alone and in no other",
  "offline-edits.e2e.ts": "document transport, IndexedDB recovery and read-only admissions",
  "overlapping-marks.e2e.ts": "two readers' marks and the server's anchor refresh",
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
  "polish.e2e.ts": "opens the margin ask composer on the live document",
  "prefix-links.e2e.ts": "`sql`-forged id twins and the server's short-id resolution",
  "progress.e2e.ts":
    "it asserts the progress the server computes over an issue's children before it opens a page",
  "project.e2e.ts": "the reference closure is the server's graph walk",
  "recipients-claim.e2e.ts": "claims and routes the server computes, with live card updates",
  "reference-cards.e2e.ts": "opens the spec's live editor and its margin thread",
  "reference-titles.e2e.ts": "asserts drawn titles inside the live editor",
  "references.e2e.ts": "the backlink index the server rebuilds after each write",
  "refetch-budget.e2e.ts": "writes after load, counted reads and the server's event feed",
  "refresh-freshness.e2e.ts": "writes after load must reach the open page within 2 s",
  "result-commit.e2e.ts": "the result line is read in the document margin's Pinned tab",
  "search.e2e.ts": "the server's search hits; the server's answer for the query",
  "settings.e2e.ts": "mappings, defaults and architecture sources saved, refused and re-read",
  "shell.e2e.ts": "the server must revoke the session",
  "signin.multiprocess.e2e.ts": "a sign-in started on one server and completed on another",
  "signins.e2e.ts": "the broker's machine list and a Revoke decided there",
  "smoke.e2e.ts":
    "the reader nobody signed in, where the stubbed layer's page is always signed in because its recording answers /auth/whoami",
  "spec-suggestion-decoration.e2e.ts": "reads the decoration's DOM inside the live editor",
  "task-items.e2e.ts": "asserts inside the live document editor",
  "text-typed-blocks.e2e.ts": "the live document editor, including typing a new option",
  "threads-order.e2e.ts":
    "two live documents, each with its own room; the margin sits on the live document",
  "threads.e2e.ts":
    "replies, resolve and reopen, edits, retractions and suggestion accepts, all server writes",
  "two-builds.multiprocess.e2e.ts":
    "two server builds, `sql` on the process registry and live document rooms",
  "typed-block-content-rule.e2e.ts": "pastes into the live document editor",
  "typed-blocks.e2e.ts": "asserts inside the live project-document editor",
  "verification.e2e.ts":
    "opens the document's live room; currency is the server's comparison with the PR head",
  "visual.e2e.ts":
    "it compares a page against a committed baseline inside the pinned Playwright image, which is its own pair of projects",
  "workspace.e2e.ts": "it asserts the counts and the titles the server groups a workspace into",
  "writer-spelling.e2e.ts": "asserts the readings inside the live document editor",
  "writes.e2e.ts":
    "a failed pin's retry, one comment from a double Submit, and a pin removed from Pinned",
};

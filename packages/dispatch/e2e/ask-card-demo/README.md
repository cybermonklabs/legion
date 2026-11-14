# Ask card demo

A live demo of Rewrite and Define on the ask card: the Dispatch web app, run by Vite, with a
stubbed back end in the browser. It is a prototype for the owner's approval, not part of the
browser suite, and not for merge.

Every word of the asks, the documents, the rewrites and the definitions is invented. The
fixtures are in the repository, and a note in the bottom right corner of every page says that
the back end is stubbed and the text invented.

## What it shows

- The card's split button: Answer, a thin rule, and a chevron that opens one menu with Rewrite
  (`r`), Ask back (`b`), Hand this question to… (`a`), Decline to answer (`n`) and Define
  selected words (`d`). An approval request's menu holds Ask back, Hand this question to and
  Define.
- One badge for a rewrite: **Rewriting…**, **Rewritten** (press it, or `w`, for the original
  wording in place; it then reads **Original**), and **Rewrite failed** (press it, or `r`, to
  try again; why it failed is the badge's tooltip and nowhere else). The ask Dispatch's check
  rewrote before the reader saw it wears the same badge.
- A rewritten decision block in a document: the block's header carries the badge, and the badge
  or `w` shows the agent's question and options in the block in place of the document's text,
  which still holds the rewrite.
- The chevron beside the asking agent's name opens a popover: the session's handles to copy, and
  under **Reaches N** every session a person's answer and replies reach, each with its Unfollow
  or Unsubscribe.
- Define: select words in an ask's question or options and press `d` (or the menu item). A small
  popover beside the words says what they mean, from the issue's record, with a link to where
  the documents define them; the asking agent's own words join it under the agent's name.
- Define in a document: the selection bar of a document's read view (the approval document in
  the Inbox's reading pane) and of its edit view (an open issue's spec) offers Define. In the read
  view `d` defines the selection. In the edit view, where letters type, the key is **Ctrl+Alt+Y**,
  **Cmd+Alt+Y** on a Mac: neither the browser, macOS nor the app uses it (Cmd+Alt+D shows and
  hides the Dock, and Cmd+Alt+I, J, C, U, K and E open the browsers' developer tools).
- The shortcut help (`?`) lists the card's keys under **Ask card**.

The fixtures to try it on (project PANE, signed in as `alice`):

- PANE-5 "R3 flip audit: go with B per the train?": Rewrite goes through; Define on "flip
  audit", "the train" or "refused lease" answers from the record and then from the agent.
- PANE-3 "Which service moves first?": the first Rewrite fails, as one that changed an option's
  label would; the second goes through.
- PANE-2: the ask Dispatch's check rewrote before it reached the Inbox.
- PANE-1: an approval request; its document reads in the Inbox's reading pane, and its card is on
  the issue's Conversation tab.
- PANE-6: a decision block in the issue's spec that the check rewrote (`/issues/PANE-6/spec`).

## Files

- `web/vite.demo.config.ts`: Dispatch's Vite config with the stub put into the page before the
  app's entry (`web/src/demo/install.ts`), so no app file imports demo code. The dev server
  proxies nothing.
- `web/src/demo/`: the stub.
  - `install.ts` replaces `fetch` and `WebSocket`, and records any request no route answers in
    `window.__dispatchDemoUnanswered`.
  - `clock.ts` fixes the page's clock at the fixtures' moment (`DEMO_CLOCK`), so every age reads
    the same whenever the demo is opened.
  - `fixtures.ts` holds the project, the issues, the asks, the rewrites and the definitions;
    `document-fixtures.ts` holds each issue's documents.
  - `server.ts` answers the Inbox, the ask routes, Rewrite (`POST /api/v1/asks/{id}/rewrite`)
    and Define (`POST /api/v1/definitions`, `GET /api/v1/definitions/{id}`); `documents.ts`
    answers an issue page and its documents; `http.ts` holds the routes' shapes.
  - `live.ts` is `/ws/live` (the event feed) and hands a document's socket to `document-room.ts`,
    which builds each document from its markdown and acknowledges every edit.
  - `banner.ts` is the corner note; `file-router.ts` is the single file's router.
- `e2e/ask-card-demo/shots.ts` is the screenshot run; `single-file.ts` builds the single file.

Writes change the stub's state, which the tab keeps in sessionStorage across a reload; a new tab
starts from the fixtures again.

## Run the demo

```sh
bun install
cd packages/dispatch/web
bunx vite --config vite.demo.config.ts --port 5191 --host 127.0.0.1
```

Open `http://127.0.0.1:5191/`. `/?item=<ask id>` opens one question in the reading pane, with the
ids in `web/src/demo/fixtures.ts`.

## Take the pictures

With the demo running:

```sh
cd packages/dispatch
bun e2e/ask-card-demo/shots.ts --base http://127.0.0.1:5191 --out "$TMPDIR/ask-card-shots" \
  --chromium <path to Chromium> [--only <name part>,…]
```

It writes one PNG per picture, `ask-card-NN-<what>.png`, at 1440x900 and the phone ones at
390x844, and `report-design.json`. The run fails if a picture did not reach its state in time,
the stub left a request unanswered, the page threw, or a check failed.

The app's font is Inter. A machine without it draws the pictures in a fallback font; to draw them
as the app is designed, put Inter's TTF files in a directory and point Chromium's fontconfig at it
with `FONTCONFIG_FILE` (a `fonts.conf` that includes `/etc/fonts/fonts.conf` and adds that
directory).

## Build the single file

```sh
cd packages/dispatch
bun e2e/ask-card-demo/single-file.ts --out "$TMPDIR/ask-card-demo.html"
bun e2e/ask-card-demo/shots.ts --file --base "file://$TMPDIR/ask-card-demo.html" \
  --out "$TMPDIR/ask-card-shots" --chromium <path to Chromium>
```

The first command makes one Vite build (`DISPATCH_DEMO_SINGLE_FILE=1`: one script with every
asset inlined) and writes the script, the stylesheet and the icon into the page, which then opens
from the disk in Chrome with no network. No service worker runs at a `file:` address, so the stub
replaces `fetch` and `WebSocket` in the page, and the router reads the address's fragment
(`#/issues/PANE-5/spec`).

The second command opens the file with the network off and checks that it opens on the Inbox,
that the menu lists its five actions, that a rewrite and Define answer, and that an issue's spec
opens by its fragment address.

## What the stub does differently from the server the design describes

- A rewrite answers its POST when it ends, about 2.4 seconds later, with the ask as the rewrite
  left it. The server runs it behind the write and writes an `ask.edited` event the page reads.
- The rewritten wording and each definition are the fixtures' own, not a model's.
- The asking agent's answer to a definition joins it 3.5 seconds after the request; the server
  sends the running session a side question and adds its answer when it comes.
- The ask read's `rewrite` field (running, done or failed, who asked, the original wording) is
  the prototype's own; no Dispatch contract holds it yet.
- Document edits are acknowledged and kept for the tab only, and every past version of a
  document reads as its current text.
- The rewritten decision block comes already rewritten: its spec holds the rewrite as its live
  text. The server's rewrite of a block writes that new version of the document itself.
- An Unsubscribe on the Reaches list lasts until the page reloads, and an Unfollow for the tab;
  the server cuts the route and tells the session.

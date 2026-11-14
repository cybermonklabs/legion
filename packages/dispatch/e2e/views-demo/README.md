# Views demo

A live demo of the Issues tab's views (dispatch://INFRA-3221): the Dispatch web app, run by Vite,
with a stubbed back end in the browser that answers every read from fixtures built out of
production reads. It is a prototype for a spec's approval, not part of the browser suite.

The fixtures, the pictures and the single file are written outside the repository. Production
data (issue titles, keys, people) never goes into a file here.

## Files

- `web/vite.demo.config.ts`: Dispatch's Vite config with the stub put into the page before the
  app's entry (`web/src/demo/install.ts`), so no app file imports demo code. The dev server
  serves the fixtures from `DISPATCH_DEMO_FIXTURES` and proxies nothing.
- `web/src/demo/`: the stub.
  - `clock.ts` fixes the page's clock: the page reads the time as the fixtures' read time
    (`manifest.daemon_read_at`) plus the time since it loaded, so every age (Waited, In phase,
    Closed) and Finished's 28-day window look the same whenever the demo is opened.
    `vite.demo.config.ts` writes that time into the page before any module runs.
  - `install.ts` replaces `fetch` and `WebSocket`.
  - `server.ts` answers every `/api/` and `/auth/` request the page makes. The listing applies
    every filter the server applies. Writes change its state, which the tab keeps in
    sessionStorage. A write goes out on the event feed as the server's event would.
  - `live.ts` is `/ws/live`: the `legion` view gets the board, the event feed gets the stub's
    events.
  - `banner.ts` is the note at the foot of every page that names what is real, estimated and
    invented.
  - `file-router.ts` is the single file's router.
- `e2e/views-demo/`:
  - `fixtures.ts` builds the fixtures.
  - `production.ts` makes and keeps the production reads.
  - `board-rules.ts` holds the board route's rules, ported from the Go of cybermonklabs/legion#3578
    (`legion/board_rules.go`, `legion/waiting_line.go`, `legion/tree.go`,
    `api/legion_board.go`).
  - `phase-facts.ts` derives the per-issue facts from Dispatch events, by the metrics' rule for
    an issue the sampler never observed.
  - `shots.ts` is the screenshot run.
  - `single-file.ts` builds the single HTML file.

## Build the fixtures

The reads are GETs only, made one at a time and at most five a second. A second run reads only
what `<out>/raw/` lacks. Add `--refresh` to read everything again.

```sh
cd packages/dispatch
DISPATCH_URL=<Dispatch base URL> DISPATCH_TOKEN_FILE=<token file> \
  bun e2e/views-demo/fixtures.ts --out "$TMPDIR/views-demo/fixtures" \
  --project <KEY> --reader <login> --daemon-url <the project's Legion daemon base URL> \
  --no-daemon-project <KEY>
```

`--project` is the project with a Legion daemon. `--daemon-url` reads its daemon's
`GET /legion/v1/state` (in the cluster, no token). Use `--daemon-state <file>` instead to build
from a saved state. Each `--no-daemon-project` adds a real project's open and done listings and
no Legion setting, so the views of a project with no daemon can be shown beside it; its done rows
keep a null `closed_at`, since their events are not read. `<out>/manifest.json` names, for each
fixture, its source, the UTC time it was read, and which of its fields are real, derived or
invented. The board leaves out a card's `checks_verdict`, which
cybermonklabs/legion#2731's newest head removes.

## The fixtures as an archive

The files the stub reads, the manifest with them, are also kept as one archive (it holds
production data, so it stays outside the repository). To run the demo or the pictures from it,
unpack it into a directory and name that directory as below:

```sh
mkdir -p "$TMPDIR/views-demo/fixtures"
tar -xzf <the archive>.tar.gz -C "$TMPDIR/views-demo/fixtures"
```

To make the archive again from a fixtures directory:

```sh
tar -czf <the archive>.tar.gz -C "$TMPDIR/views-demo/fixtures" \
  manifest.json issues.json population.json projects.json reader.json schema.json agents.json
```

## Run the demo

```sh
cd packages/dispatch/web
DISPATCH_DEMO_FIXTURES="$TMPDIR/views-demo/fixtures" \
  bunx vite --config vite.demo.config.ts --port 5190 --host 127.0.0.1
```

Open `http://127.0.0.1:5190/projects/<KEY>/issues`. A request the stub cannot answer is logged
in the console (`[views demo] no stub route answers …`) and kept in
`window.__dispatchDemoUnanswered`.

To show a failed read of the daemon, add `demo=read-failed` to the address
(`/projects/<KEY>/issues?view=open&demo=read-failed`; in the single file, after the `#`). The
board's newest read is then a failure with an error text the demo makes up, its cards those of
the read before it, so the alert under the tabs shows; the banner names the failure among what
is invented. The flag leaves the address before the app reads it and holds for the rest of the
tab's life; `demo=read-ok` turns it off.

To show a failed read of the Legion setting, add `demo=settings-failed` the same way
(`/projects/<KEY>/issues?view=open&demo=settings-failed`). `GET /api/v1/settings/legion` then
answers 503 with an error text the demo makes up, so the page draws the project as one with no
daemon, with the "Legion setting read failed" alert and its Retry under the tabs, and the banner
names the failure. It holds for the rest of the tab's life; `demo=settings-ok` turns it off.

## Take the pictures

With the demo running:

```sh
cd packages/dispatch
TZ=UTC bun e2e/views-demo/shots.ts --base http://127.0.0.1:5190 --out "$TMPDIR/views-demo/shots" \
  --chromium <path to Chromium> [--before] [--no-daemon-project <KEY>]
```

Every page runs in the UTC time zone and the `en-US` language, and its clock stands at the
fixtures' read time (`web/src/demo/clock.ts`), so a run on another machine or another day draws
the same times. `report-<mode>.json` writes down the Chromium version and every font the browser
drew the pictures' text in, with its glyph count, so two runs can be compared.

Without `--before` it takes the design's pictures, each named by what it shows:

- every built-in view, and All open scrolled to the table's right end;
- In flight and All open on the Board;
- Finished with Time per phase in Hours;
- Save view, the panel and then the saved tab;
- the Columns menu;
- the Closed date filter open;
- the project settings page;
- the (i) hint open beside the tabs;
- five phone pictures: All open and In flight in the table, the In flight and All open Boards,
  and Time per phase in Hours;
- All open, In flight and All open on the Board of the project with no Legion daemon
  (`--no-daemon-project`, GTM by default);
- All open with the daemon's read failed (`demo=read-failed`), and with the Legion setting's
  read failed (`demo=settings-failed`).

`--before` takes the page before the design, on a checkout of its base: the List, the Board, the
Legion view and the phone List. A picture is taken only once its page has drawn its rows (a
table row, a Board card or a Legion view card) or its own element; a page that does not draw
them in 30 s fails its picture. The design's Issues page is also waited on for its view tabs
(`[data-view-tab]`), which the page before the design has none of, so the design's pictures
taken against that page fail at the first one. The run fails if a picture failed, the stub left
a request unanswered, the page threw, or the phone banner took more than two lines.
`report-<mode>.json` in the output lists all of it.

## Build the single file

```sh
cd packages/dispatch
bun e2e/views-demo/single-file.ts --fixtures "$TMPDIR/views-demo/fixtures" \
  --out "$TMPDIR/views-demo/<name>.html"
TZ=UTC bun e2e/views-demo/shots.ts --file --base "file://$TMPDIR/views-demo/<name>.html" \
  --out "$TMPDIR/views-demo/shots" --chromium <path to Chromium>
```

The first command makes one Vite build (`DISPATCH_DEMO_SINGLE_FILE=1`: one script with every
asset inlined, `inlineDynamicImports`). Then it writes the script, the stylesheet, the icon and
the fixtures into the page. The page opens from the disk with no network. No service worker runs
at a `file:` address, so the stub replaces `fetch` and `WebSocket` in the page. The router reads
the address's fragment, the one part of a `file:` address a page may change.

The second command opens the file with the network off and checks that:

- the file opens on the Issues tab, and a view tab opens its view;
- the Board draws cards;
- a drag on the Board sets a status;
- Save view adds a tab, and its delete takes the tab away, also after a reload;
- the Columns menu shows a column it ticks;
- the Closed filter's 7 days lists the done issues with Legion work closed in the last 7 days.

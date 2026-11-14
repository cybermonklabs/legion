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
  --project <KEY> --reader <login> --daemon-url <the project's Legion daemon base URL>
```

`--daemon-url` reads the daemon's `GET /legion/v1/state` (in the cluster, no token). Use
`--daemon-state <file>` instead to build from a saved state. `<out>/manifest.json` names, for
each fixture, its source, the UTC time it was read, and which of its fields are real, derived or
invented.

## Run the demo

```sh
cd packages/dispatch/web
DISPATCH_DEMO_FIXTURES="$TMPDIR/views-demo/fixtures" \
  bunx vite --config vite.demo.config.ts --port 5190 --host 127.0.0.1
```

Open `http://127.0.0.1:5190/projects/<KEY>/issues`. A request the stub cannot answer is logged
in the console (`[views demo] no stub route answers …`) and kept in
`window.__dispatchDemoUnanswered`.

## Take the pictures

With the demo running:

```sh
cd packages/dispatch
bun e2e/views-demo/shots.ts --base http://127.0.0.1:5190 --out "$TMPDIR/views-demo/shots" \
  --chromium <path to Chromium> [--before]
```

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
  and Time per phase in Hours.

`--before` takes the page before the design, on a checkout of its base: the List, the Board, the
Legion view and the phone List. The run fails if the stub left a request unanswered or the page
threw. `report-<mode>.json` in the output lists all of it.

## Build the single file

```sh
cd packages/dispatch
bun e2e/views-demo/single-file.ts --fixtures "$TMPDIR/views-demo/fixtures" \
  --out "$TMPDIR/views-demo/<name>.html"
bun e2e/views-demo/shots.ts --file --base "file://$TMPDIR/views-demo/<name>.html" \
  --out "$TMPDIR/views-demo/shots" --chromium <path to Chromium>
```

The first command makes one Vite build (`DISPATCH_DEMO_SINGLE_FILE=1`: one script with every
asset inlined, `inlineDynamicImports`). Then it writes the script, the stylesheet, the icon and
the fixtures into the page. The page opens from the disk with no network. No service worker runs
at a `file:` address, so the stub replaces `fetch` and `WebSocket` in the page. The router reads
the address's fragment, the one part of a `file:` address a page may change.

The second command opens the file with the network off and checks four things:

- a view tab opens its view;
- the Board draws cards;
- a drag on the Board sets a status;
- Save view adds a tab.

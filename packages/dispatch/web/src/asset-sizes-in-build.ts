import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

/**
 * The largest file the server keeps of a dashboard build: `routes.MaxRetainedAssetSize` in
 * `packages/envoy/internal/dispatch/routes/assets.go`. Every server process publishes its build's
 * assets to Dispatch's database at boot, so another build's tab can load them from it, and refuses
 * to boot on a build holding a larger file (`dispatch: publish dashboard assets`), which stalls a
 * rolling update. This check refuses such a build in CI instead.
 */
export const MAX_ASSET_BYTES = 8 * 1024 * 1024;

/**
 * The largest entry chunk a build may carry: the one `dist/index.html` names in its module
 * script, which every page of the dashboard parses before it draws anything, and which no cache
 * spares a new browser context (so each of the browser suite's rows pays it too).
 *
 * 500,000 bytes is Vite's own default `chunkSizeWarningLimit`, which `web/vite.config.ts` raised
 * to 1,200 kB on 2026-10-29 rather than split the entry. That is how this number grew unseen:
 * the entry was 781,007 bytes beside that line and measured 1,014,930 on 2026-11-11, and nothing
 * in the tree refused the 233,923 bytes between them (R12 of dispatch://INFRA-1109/spec).
 *
 * THIS BUDGET REFUSED PULL REQUEST #3513'S OWN BUILD when it was written, on purpose: the entry
 * was 1,028,690 bytes then, and a budget written only once the number already fits is a budget
 * nobody has seen refuse anything. The shell was split after it (R13 of the same spec), and
 * `bun run build:web` at `61e5c402` wrote an entry of 485,658 bytes (the one
 * `web/dist/assets/index-*.js` that `dist/index.html` names, measured 2026-11-16), which left
 * 14,342 bytes of room under this budget. The split itself wrote 491,264; the 5,909 bytes after
 * it are what lets an issue page wait for its document rather than fetch beside it: the loaders
 * of the issue page's tab chunks (`features/issue/tab-chunks.ts`), the shell's hold on its
 * extras (`features/shell/content-drawn.ts`), and the chunk component that draws a chunk already
 * in with no `Suspense` stage (`features/shell/chunk-component.tsx`). A forward merge of the
 * default branch then put 3,047 more in the shell's path, and this budget refused that build at
 * 500,220 bytes, as it is here to. The boundary that answered it is the headless Markdown
 * engine, whose one importer in the entry was the warm every page starts at boot: fetched there
 * as a chunk of its own (`features/shell/DeploymentResilience.tsx`), it took 13,554 bytes out,
 * and the commits and forward merges after it moved the entry by about a thousand bytes, most
 * of it down (the landing seed's query factories; 485,658 bytes at `61e5c402` on 2026-11-16).
 * This budget refused five forward merges since: the three below, and two more further on. The
 * merge of `d079920d`, the default branch's
 * own frame for the new issue page (2026-11-19), wrote 508,066 bytes, and `b18ea8d5` the same day
 * answered it by loading that frame's shell and the pending-writes indicator as chunks; the merge
 * of `bafd3618`, which brought the default branch's draft-keeping (2026-11-20), wrote 519,305,
 * and `9ccacce3` the same day answered it by making the kinds of write a chunk the take-up waits
 * for and taking the draft hook out of the entry. At `381b29ad` (2026-11-20) the entry was
 * 496,982 bytes, 3,018 under this budget, at `1293f3d9` the same day 497,475, and at `06c1e7b2`
 * (2026-11-21), built with a 40-character build id as CI builds it, it is 497,539 bytes, 2,461
 * under. The third, the merge of `4b22ece4` (2026-11-23, the default branch's dialog focus, #4985),
 * wrote 500,672, and `01fc0e41` the same day answered it with two moves, to 499,380. The Inbox's
 * `Blocked on you` strip and the project page's pill left the entry for a file of their own,
 * `features/inbox/BlockedOnYouLink.tsx`, a chunk of 866 bytes the Inbox's and the project page's
 * chunks share; without that move the entry at `01fc0e41` measured 500,104 bytes. And the
 * modulepreload polyfill, 710 bytes of the entry, is off, for the reason beside `modulePreload`
 * in `web/vite.config.ts`. `97c0cde6`'s focus helper then added 105, so the build at `4935c4af`,
 * with a 39-character build id, read 499,485 bytes, 515 under this budget, and the same at the
 * merges `4548096d` and `e15ee5e4` (CI's 40-character id writes one byte more, by the rule
 * below). On that day, 2026-11-23, the default branch's own build at `d795b25c` wrote 1,144,188
 * bytes.
 * This budget refused the loading box and the alert of the surfaces a key opens too
 * (`features/shell/asked-surface.tsx`, review 6034817973; `4afe19c7`, 2026-11-24, 500,530 bytes),
 * and `4e9171bc` the same day answered it by taking `features/refs/AgentName.tsx` out of the
 * entry (497,401 bytes): the rail's pinned agents draw a session from that module's session half
 * alone (`features/refs/SessionName.tsx`), so the rest of it is a chunk the pages' chunks share.
 * At `540b1fe2`, after the changes since, Details' one Escape rule among them
 * (`escapeClosesDrawer`, `features/shell/useDialog.ts`), the entry was 497,933 bytes, the two
 * lists' alert for a render error (`features/writes/write-hosts.tsx`) takes it to 498,553, and the
 * sign-in page's list of kept writes, in the entry so that its warning depends on no chunk
 * (`features/writes/KeptWrites.tsx`), takes it to 499,583, each with a 39-character build id. The
 * merge of the default branch's correction and deletion of a post (#3512, 2026-11-24) then wrote
 * 500,975, and three moves the same day took code that no first paint draws out of the entry,
 * none of them a new lazy load, to 498,776:
 * - a held send refreshes a comment list under `ownerCommentsQuery`'s key, so
 *   `features/conversation/composer-model.ts` is out of the entry (411 bytes);
 * - the rail's state mark reads its relative time from `features/refs/relative-time.ts`, so
 *   `<Timestamp>` is a chunk the pages' chunks share (646 bytes);
 * - the recent items read a document's route from `features/refs/routes.ts`, so the search bar's
 *   chip (`features/search/bar-scope.ts`) is a chunk that the bar and the issue list share
 *   (1,142 bytes).
 * With the correction's mark in the event map's chunk (`api/event-refresh-load.ts`) the entry read
 * 498,590 bytes with a 39-character build id. The forward merges of the default branch after it,
 * whose layers of the page's foot (#4631) added about 6,600 bytes (499,088 before the merge of
 * `c227d046` and 505,685 after it), and the fixes after them wrote
 * 506,935, over this budget again. The page addresses the shell reads then left
 * `features/refs/routes.ts` for a module of their own (`features/refs/page-routes.ts`), with no
 * new lazy load, so the grammar of references and the modules only it imports left the entry as
 * two chunks that the pages' chunks share (`routes-*.js`, 10,989 bytes, and
 * `dispatch-block-ref-*.js`, 3,632), and the entry read 492,802 bytes with a 39-character build
 * id on 2026-11-25, 7,198 under this budget. The simplify pass of pull request #3513 the same day
 * took it to 492,840 bytes with a 39-character build id, 7,160 under this budget. The forward merge
 * of the default branch's `479c1460` the same day took it to 492,868 bytes, 7,132 under this
 * budget: its new event type (`project.access_changed`, #4674) is a member of the list of event
 * types (`packages/contracts/src/dispatch-api-constants.ts`) that the entry reads.
 * No hash is recorded with these sizes: that hash carries the build id, which falls back to the clock
 * (`__DISPATCH_BUILD__`, `web/vite.config.ts`), so two builds of one tree write the same bytes
 * under two names. The id is in the entry too, so CI's build, whose id is the 40-character
 * commit, writes an entry 27 bytes longer than a local build's, whose id is 13 digits of the
 * clock. A change that adds to what every page parses spends that room and then fails
 * `bun run check:asset-sizes`, which is the point: the next shell boundary is cheaper to find
 * now than the 233,923 bytes above were.
 *
 * The budget is the entry's alone and not every chunk's: the Mermaid worker (1,565,608 bytes)
 * and the ECharts chunk (1,134,772) are larger and no landing fetches either, so a budget over
 * every chunk would refuse those two first and say nothing about the bytes a page waits on.
 * `MAX_ASSET_BYTES` above is what holds the whole build to what the server will keep.
 *
 * Read it again: `bun run build:web && bun run check:asset-sizes` in `packages/dispatch`. The
 * three sizes of `61e5c402` above - the entry, the Mermaid worker and the ECharts chunk - were
 * measured that way on 2026-11-16, and the entry sizes after them are this check's readings at
 * the commit or merge each names; the two older entry sizes are the readings of 2026-10-29 and
 * 2026-11-11 named above. The Mermaid worker and the ECharts chunk read the same at `53286717`
 * on 2026-11-23, seven days after `61e5c402` (`web/vite.config.ts`, beside
 * `chunkSizeWarningLimit`, which keeps no size of the entry and names this file for it).
 */
export const MAX_ENTRY_BYTES = 500_000;

const DIST_DIR = join(import.meta.dir, "..", "dist");
const DIST_ASSETS_DIR = join(DIST_DIR, "assets");

export interface OversizedAsset {
  /** The file's path below the assets directory, slash-separated. */
  path: string;
  bytes: number;
}

/** Every file below `dir`, at any depth, larger than `limit` bytes, in path order. */
export function oversizedAssets(dir: string, limit = MAX_ASSET_BYTES): OversizedAsset[] {
  const oversized: OversizedAsset[] = [];
  for (const name of readdirSync(dir, { recursive: true, encoding: "utf8" })) {
    const stat = statSync(join(dir, name));
    if (stat.isFile() && stat.size > limit) {
      oversized.push({ path: name.replaceAll("\\", "/"), bytes: stat.size });
    }
  }
  return oversized.sort((a, b) => a.path.localeCompare(b.path));
}

export interface EntryChunk {
  /** The chunk's path below the build directory, as `index.html` names it, slash-separated. */
  path: string;
  bytes: number;
}

/**
 * The entry chunk of the build at `dist`, with its size: the file `index.html` names in its
 * module script. That is the one script the document itself loads - every other chunk is an
 * import of it or a lazy piece fetched later - so it is what a cold page waits for.
 *
 * `index.html` also carries an inline script (the chunk-failure reload beside the entry), which
 * has no `src` and is none of this reading; a build naming no module script, or more than one,
 * is refused rather than measured, since which of them a page would wait for is then unknown.
 *
 * HTML reads tag and attribute names in any case and a value in double, single or no quotes, so
 * this reading does too: a `<SCRIPT TYPE=module SRC=…>` the matcher passed over would leave the
 * budget measuring nothing it could refuse.
 */
export function entryChunk(dist: string): EntryChunk {
  const html = readFileSync(join(dist, "index.html"), "utf8");
  const sources = [...html.matchAll(/<script\b[^>]*>/gi)]
    .filter((tag) => /\btype\s*=\s*["']?module\b/i.test(tag[0]))
    .map((tag) => /\bsrc\s*=\s*(?:"([^"]+)"|'([^']+)'|([^\s"'>]+))/i.exec(tag[0]))
    .map((match) => match?.[1] ?? match?.[2] ?? match?.[3])
    .filter((source) => source !== undefined);
  if (sources.length !== 1) {
    throw new Error(
      `${join(dist, "index.html")} names ${sources.length} module scripts; a build names exactly one, which is its entry chunk.`
    );
  }
  const path = sources[0].replace(/^\//, "");
  return { path, bytes: statSync(join(dist, path)).size };
}

/** One budget's reading of a build: the line the run prints for it, and whether that line is a
 *  refusal. */
export interface BudgetReading {
  message: string;
  refused: boolean;
}

/**
 * Both budgets read over the build at `dist`, in the order the run prints them, and nothing
 * printed or exited here: this is what the check decides, and `main` below is what says it.
 *
 * Both are read whatever either finds, so a build over on each is told about each in one run.
 * The verdict is the readings' own - the run refuses when any reading does - which is what the
 * tests hold: a row over the entry budget that returned no refusal would be a build CI waves
 * through.
 */
export function budgetReadings(dist: string): BudgetReading[] {
  const readings: BudgetReading[] = [];
  const oversized = oversizedAssets(join(dist, "assets"));
  for (const asset of oversized) {
    readings.push({
      message: `web/dist/assets/${asset.path} is ${asset.bytes} bytes, over the ${MAX_ASSET_BYTES}-byte limit: the server refuses to boot on this build.`,
      refused: true,
    });
  }
  if (oversized.length === 0) {
    readings.push({
      message: `Every file of web/dist/assets is at most ${MAX_ASSET_BYTES} bytes.`,
      refused: false,
    });
  }
  const entry = entryChunk(dist);
  if (entry.bytes > MAX_ENTRY_BYTES) {
    readings.push({
      message: `web/dist/${entry.path} is ${entry.bytes} bytes, over the ${MAX_ENTRY_BYTES}-byte entry budget: it is the chunk web/dist/index.html names in its module script, so every page of the dashboard parses all of it before it draws anything, and a new browser context has none of it cached.`,
      refused: true,
    });
  } else {
    readings.push({
      message: `The entry chunk web/dist/${entry.path} is ${entry.bytes} bytes, inside the ${MAX_ENTRY_BYTES}-byte budget.`,
      refused: false,
    });
  }
  return readings;
}

function main(): void {
  if (!existsSync(DIST_ASSETS_DIR)) {
    throw new Error(
      `No built assets found at ${DIST_ASSETS_DIR}. Run "bun run build:web" before "bun run check:asset-sizes".`
    );
  }
  const readings = budgetReadings(DIST_DIR);
  for (const reading of readings) {
    if (reading.refused) console.error(reading.message);
    else console.log(reading.message);
  }
  if (readings.some((reading) => reading.refused)) {
    process.exit(1);
  }
}

if (import.meta.main) {
  main();
}

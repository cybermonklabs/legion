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
 * THIS BUDGET REFUSED THIS BRANCH'S OWN BUILD when it was written, on purpose: the entry was
 * 1,028,690 bytes then, and a budget written only once the number already fits is a budget
 * nobody has seen refuse anything. The shell was split after it (R13 of the same spec), and
 * `bun run build:web` at this head writes an entry of 490,184 bytes (the one
 * `web/dist/assets/index-*.js` that `dist/index.html` names, measured 2026-11-12), which leaves
 * 9,816 bytes of room under this budget. No hash is recorded with it: that hash carries the
 * build id, which falls back to the clock (`__DISPATCH_BUILD__`, `web/vite.config.ts`), so two
 * builds of one tree write the same bytes under two names. A change that adds to what every
 * page parses spends that room and then
 * fails `bun run check:asset-sizes`, which is the point: the next shell boundary is cheaper to
 * find now than the 233,923 bytes above were.
 *
 * The budget is the entry's alone and not every chunk's: the Mermaid worker (1,565,608 bytes)
 * and the ECharts chunk (1,134,772) are larger and no landing fetches either, so a budget over
 * every chunk would refuse those two first and say nothing about the bytes a page waits on.
 * `MAX_ASSET_BYTES` above is what holds the whole build to what the server will keep.
 *
 * Read it again: `bun run build:web && bun run check:asset-sizes` in `packages/dispatch`. The
 * three sizes of this head were measured that way on 2026-11-12; the two older entry sizes are
 * `web/vite.config.ts`'s own record of 2026-10-29 and 2026-11-11.
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
 */
export function entryChunk(dist: string): EntryChunk {
  const html = readFileSync(join(dist, "index.html"), "utf8");
  const sources = [...html.matchAll(/<script\b[^>]*>/g)]
    .filter((tag) => /\btype="module"/.test(tag[0]))
    .map((tag) => /\bsrc="([^"]+)"/.exec(tag[0])?.[1])
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

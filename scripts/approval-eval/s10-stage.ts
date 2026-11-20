import { createHash } from "node:crypto";
import { mkdir, readdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { CannotRunError, recordOf } from "./corpus-io";
import { type DispatchConfig, readBytes } from "./dispatch-read";
import { BUILD_CHAIN_NAMES, chainRefusal } from "./draw-io";
import {
  FILE_KEY,
  INPUTS_VERSION,
  INVENTORY_FILE,
  PAGES_DIRECTORY,
  type PageRecord,
  pageHitOf,
  pagePath,
  pageRecordOf,
  type QueryHit,
  type QueryInputs,
  REQUESTS_DIRECTORY,
  requestPath,
  STAGE_FILE,
  type StageChain,
  type StageCounts,
  type StageRecord,
  stageRecordOf,
} from "./s10-inputs-io";

/**
 * What the claim's S10 inputs stage (`s10-inputs.ts`) keeps on disk, and how: the stage record
 * and the files it names (`Stage`), written so a crash never leaves a half-written file counted
 * as written and a resume takes only what its own claim run's record names; and the search pages
 * S10's neighbours read (`Searches`), each read once, named by the sha256 of its bytes and
 * archived before it is used. The shapes these write are `s10-inputs-io.ts`'s; this module is the
 * writer's alone, and no later stage imports it.
 */

/** The inputs cannot be written as the registered text fixes them, so the command stops. */
export class S10InputsError extends CannotRunError {
  override readonly name = "S10InputsError";
}

/** How many hits one search page asks for, as S10 asks. */
const SEARCH_PAGE = 50;

/** The sha256 of some bytes, as every record here names a file by. */
function sha256Of(bytes: Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** The fields of a `StageChain` outside its claim run and draw, as a refusal names them. */
const STAGE_CHAIN_NAMES: Readonly<Partial<Record<keyof StageChain, string>>> = {
  reference: "read",
  runDigest: "run digest",
  person: "person",
  throughEventId: "pin",
  reserveAsk: "reserve-ask.ts closure",
};

/** The fields of a `StageChain`'s draw, as a refusal names them. */
const DRAW_CHAIN_NAMES: Readonly<Record<keyof StageChain["draw"], string>> = {
  drawSha256: "draw.json",
  slicesSha256: "slices.json",
};

/**
 * The first field in which a stage record's chain is not `wanted`, worded as the tail of a
 * refusal, and null where it is the same chain: `chainRefusal` over the claim run and the build,
 * the read, digest, person, pin and decision, and the draw, in that order.
 */
function stageChainRefusal(found: StageChain, wanted: StageChain): string | null {
  return (
    chainRefusal(found.claimRun, wanted.claimRun, BUILD_CHAIN_NAMES) ??
    chainRefusal(found, wanted, STAGE_CHAIN_NAMES) ??
    chainRefusal(found.draw, wanted.draw, DRAW_CHAIN_NAMES)
  );
}

/** How many files this process has begun to write, which names each one's partial file. */
let begun = 0;

/** Writes a file whole or not at all, so a resume never takes a half-written one for written. */
export async function writeWhole(path: string, text: string): Promise<void> {
  begun += 1;
  const partial = `${path}.${process.pid}.${begun}.partial`;
  await writeFile(partial, text, "utf8");
  await rename(partial, path);
}

/** A stage record of a run that has written nothing yet. */
function emptyStage(chain: StageChain, startedAt: string): StageRecord {
  return {
    stage: "s10-inputs",
    version: INPUTS_VERSION,
    state: "started",
    ...chain,
    startedAt,
    finishedAt: null,
    dropScanThrough: null,
    dropScanPages: null,
    counts: null,
    inventorySha256: null,
    requests: {},
    pages: {},
  };
}

/**
 * Holds one directory of the stage to its record, before a resume takes anything from it: a file
 * written beside its place whose bytes the record names is moved into its place, every other is
 * removed unread, and then every file there must be one the record names, with the bytes it
 * names, and every file it names must be there.
 */
async function holdToRecord(
  directory: string,
  recorded: Readonly<Record<string, string>>
): Promise<void> {
  const names = (await readdir(directory).catch((): string[] => [])).sort();
  for (const name of names) {
    if (!name.endsWith(".json.partial")) continue;
    const key = name.slice(0, -".json.partial".length);
    const partial = join(directory, name);
    const bytes = await readFile(partial);
    const committed = Object.hasOwn(recorded, key) && recorded[key] === sha256Of(bytes);
    if (committed) await rename(partial, join(directory, `${key}.json`));
    else await rm(partial, { force: true });
  }
  for (const name of await readdir(directory).catch((): string[] => [])) {
    const key = /^([A-Za-z0-9-]+)\.json$/.exec(name)?.[1];
    if (key === undefined || !Object.hasOwn(recorded, key)) {
      throw new S10InputsError(
        `${join(directory, name)} is a file the stage record does not name, so no run here can ` +
          "show it is its own"
      );
    }
  }
  for (const [key, sha256] of Object.entries(recorded)) {
    const path = join(directory, `${key}.json`);
    const bytes = await readFile(path).catch(() => null);
    if (bytes === null) {
      throw new S10InputsError(
        `the stage record names ${path}, which is not there: a resume never writes a file its ` +
          "record names again"
      );
    }
    if (sha256Of(bytes) !== sha256) {
      throw new S10InputsError(
        `${path} is not the file the stage record names: its sha256 is ${sha256Of(bytes)}, and ` +
          `the record names ${sha256}`
      );
    }
  }
}

/** The record a resume took, as its bytes were, and when the run that wrote it ended. */
interface Resumed {
  text: string;
  finishedAt: string | null;
}

/**
 * The stage record and the files it names, written through one writer, so each write of the
 * record holds every file committed before it. A file is committed by writing it beside its
 * place, writing the record that names it, and moving it into its place.
 */
export class Stage {
  readonly directory: string;
  readonly record: StageRecord;
  private readonly resumed: Resumed | null;
  private saving: Promise<void> = Promise.resolve();

  private constructor(directory: string, record: StageRecord, resumed: Resumed | null) {
    this.directory = directory;
    this.record = record;
    this.resumed = resumed;
  }

  /**
   * Takes `directory` for this run: a directory holding nothing is started, with a stage record
   * naming the chain; one holding this chain's record is resumed, held to its record
   * (`holdToRecord`); anything else is refused.
   */
  static async open(directory: string, chain: StageChain): Promise<Stage> {
    await mkdir(directory, { recursive: true });
    const recordPath = join(directory, STAGE_FILE);
    const text = await readFile(recordPath, "utf8").catch(() => null);
    if (text === null) {
      const entries = await readdir(directory);
      if (entries.length > 0) {
        throw new S10InputsError(
          `${directory} holds files and no ${STAGE_FILE}, so no run here can show they are its own`
        );
      }
      const stage = new Stage(directory, emptyStage(chain, new Date().toISOString()), null);
      await stage.save();
      return stage;
    }
    const found = stageRecordOf(text, recordPath);
    const difference = stageChainRefusal(found, chain);
    if (difference !== null) {
      throw new S10InputsError(
        `${directory} holds another run's inputs: ${difference}. A resume takes only its own ` +
          "claim run's directory, over the same build and draw"
      );
    }
    const through = found.dropScanThrough;
    if (through !== null && through < chain.throughEventId) {
      throw new S10InputsError(`${recordPath} records a drop scan through no event from the pin`);
    }
    const own = [STAGE_FILE, INVENTORY_FILE, REQUESTS_DIRECTORY, PAGES_DIRECTORY];
    for (const name of await readdir(directory)) {
      if (name.endsWith(".partial")) await rm(join(directory, name), { force: true });
      else if (!own.includes(name)) {
        throw new S10InputsError(
          `${join(directory, name)} is no file this command writes, so no run here can show it ` +
            "is its own"
        );
      }
    }
    await holdToRecord(join(directory, REQUESTS_DIRECTORY), found.requests);
    await holdToRecord(join(directory, PAGES_DIRECTORY), found.pages);
    const record: StageRecord = {
      ...emptyStage(chain, found.startedAt),
      dropScanThrough: through,
      dropScanPages: found.dropScanPages,
      requests: found.requests,
      pages: found.pages,
    };
    const finishedAt = found.state === "written" ? found.finishedAt : null;
    return new Stage(directory, record, { text, finishedAt });
  }

  /** The record's text as it stands. */
  private text(): string {
    return `${JSON.stringify(this.record, null, 2)}\n`;
  }

  /** Writes the record as it stands, after any write of it already under way. */
  save(): Promise<void> {
    this.saving = this.saving.then(() => writeWhole(join(this.directory, STAGE_FILE), this.text()));
    return this.saving;
  }

  /** Commits one request file or page record file, and gives back the sha256 of its bytes. */
  async commit(kind: "requests" | "pages", key: string, text: string): Promise<string> {
    if (!FILE_KEY.test(key)) {
      throw new S10InputsError(`the ask id or page ${key} cannot name a file of its own`);
    }
    const path =
      kind === "requests" ? requestPath(this.directory, key) : pagePath(this.directory, key);
    await mkdir(dirname(path), { recursive: true });
    const partial = `${path}.partial`;
    await writeFile(partial, text, "utf8");
    const sha256 = sha256Of(text);
    this.record[kind][key] = sha256;
    await this.save();
    await rename(partial, path);
    return sha256;
  }

  /**
   * Writes the record of a run that has ended, with its counts and the sha256 of its inventory. A
   * resume of a written directory that wrote nothing new keeps the `finishedAt` of the run that
   * wrote it, so the record's bytes are the ones it resumed.
   */
  async finish(counts: StageCounts, inventorySha256: string): Promise<void> {
    this.record.state = "written";
    this.record.counts = counts;
    this.record.inventorySha256 = inventorySha256;
    this.record.finishedAt = this.resumed?.finishedAt ?? null;
    if (this.record.finishedAt === null || this.text() !== this.resumed?.text) {
      this.record.finishedAt = new Date().toISOString();
    }
    await this.save();
  }
}

/** One query's pages, by the offset each was asked at. */
export type QueryPages = ReadonlyArray<{ offset: number; page: PageRecord }>;

/**
 * The search, as S10 asks it and once for each query and offset: every page its record names is
 * taken, and every other page is read now, named by the sha256 of its bytes and committed to the
 * stage before it is used. A body that is no search answer stops the run.
 */
export class Searches {
  private readonly stage: Stage;
  private readonly config: DispatchConfig;
  private readonly pages = new Map<string, PageRecord>();
  private readonly byRead = new Map<string, PageRecord>();
  private readonly writing = new Map<string, Promise<unknown>>();
  /** How many pages this run read, beside the ones it took from an earlier run. */
  read = 0;

  constructor(stage: Stage, config: DispatchConfig, pages: readonly PageRecord[]) {
    this.stage = stage;
    this.config = config;
    for (const page of pages) {
      this.pages.set(page.sha256, page);
      for (const read of page.reads) this.byRead.set(`${read.offset}\n${read.query}`, page);
    }
  }

  /** The page records `--out` holds, each held to the stage record already. */
  static async archived(stage: Stage): Promise<PageRecord[]> {
    const pages: PageRecord[] = [];
    for (const key of Object.keys(stage.record.pages).sort()) {
      const path = pagePath(stage.directory, key);
      const record = pageRecordOf(await readFile(path, "utf8"), path);
      if (record.sha256 !== key) {
        throw new S10InputsError(`${path} records the page ${record.sha256}, not the one it names`);
      }
      pages.push(record);
    }
    return pages;
  }

  /** The pages of one query: its first, and its second where the first held a full page. */
  async pagesOf(query: string): Promise<QueryPages> {
    const first = await this.pageOf(query, 0);
    const pages = [{ offset: 0, page: first }];
    if (first.hits.length === SEARCH_PAGE) {
      pages.push({ offset: SEARCH_PAGE, page: await this.pageOf(query, SEARCH_PAGE) });
    }
    return pages;
  }

  private async pageOf(query: string, offset: number): Promise<PageRecord> {
    const key = `${offset}\n${query}`;
    const known = this.byRead.get(key);
    if (known !== undefined) return known;
    const path =
      `/api/v1/search/results?q=${encodeURIComponent(query)}` +
      `&limit=${SEARCH_PAGE}&offset=${offset}`;
    const bytes = await readBytes(path, this.config);
    const readAt = new Date().toISOString();
    const sha256 = sha256Of(bytes);
    let body: unknown;
    try {
      body = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
    } catch {
      throw new S10InputsError(`GET ${path} answered no JSON`);
    }
    const results = recordOf(body)?.results;
    if (!Array.isArray(results)) throw new S10InputsError(`GET ${path} answered no results`);
    const hits = results.map((one) => pageHitOf(one, `GET ${path}`));
    const page = this.pages.get(sha256) ?? { sha256, hits, reads: [] };
    page.reads.push({ query, offset, readAt });
    this.pages.set(sha256, page);
    this.byRead.set(key, page);
    this.read += 1;
    // One commit of a page at a time, so two reads answered by the same bytes are both recorded.
    const before = this.writing.get(sha256) ?? Promise.resolve();
    const next = before.then(() =>
      this.stage.commit("pages", sha256, `${JSON.stringify(page, null, 2)}\n`)
    );
    this.writing.set(sha256, next);
    await next;
    return page;
  }
}

/** One of a request's queries, as its pages answered it. */
export function queryInputsOf(query: string, pages: QueryPages): QueryInputs {
  const hits: QueryHit[] = [];
  for (const { page } of pages) {
    page.hits.forEach((hit, position) => {
      if (hit.kind !== "ask") return;
      hits.push({ id: hit.id, rank: hit.rank, page: page.sha256, position });
    });
  }
  return {
    query,
    pages: pages.map(({ offset, page }) => ({ sha256: page.sha256, offset })),
    hits,
  };
}

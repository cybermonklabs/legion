// The production reads the views demo's fixtures are built from (README.md beside this file). Every
// read is a GET, made one at a time and at most about five a second, and every answer is kept
// under `<out>/raw/` with the UTC time it was read, so a rebuild reads only what it lacks. The
// Dispatch server and its token come from the environment (DISPATCH_URL, DISPATCH_TOKEN_FILE),
// the daemon's address from a flag: nothing here names a host.
import { mkdir, readFile, stat, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";

/** The least time between the starts of two reads: five a second at most. */
const READ_SPACING_MS = 220;
/** The largest page the events route answers. */
const EVENT_PAGE = 200;

/** One event of an issue, in the fields the fixtures read: who wrote it, when, and the status or
 *  handoff word it carries. */
export interface ReadEvent {
  seq: number;
  type: string;
  created_at: string;
  actor_kind: string;
  actor_id: string;
  session_title: string | null;
  status: string | null;
  changed: string[] | null;
  handoff_phase: string | null;
}

/** When each kept read was made, by its file under `raw/`. */
export type ReadTimes = Record<string, string>;

export class ProductionReader {
  private lastStart = 0;
  private times: ReadTimes = {};
  private readonly timesFile: string;

  constructor(
    private readonly raw: string,
    private readonly server: string | undefined,
    private readonly tokenFile: string | undefined,
    private readonly refresh: boolean
  ) {
    this.timesFile = join(raw, "read-times.json");
  }

  async open(): Promise<void> {
    await mkdir(join(this.raw, "events"), { recursive: true });
    this.times = await readJson<ReadTimes>(this.timesFile).catch(() => ({}));
  }

  readTimes(): ReadTimes {
    return this.times;
  }

  /** The answer of `GET <DISPATCH_URL><path>`, read once and kept as `raw/<name>`. */
  async dispatch<T>(name: string, path: string): Promise<T> {
    const file = join(this.raw, name);
    if (!this.refresh && (await exists(file))) return readJson<T>(file);
    const body = await this.get<T>(path);
    await this.keep(name, body);
    return body;
  }

  /** The daemon's whole state, `GET <daemonUrl>/legion/v1/state`, kept as `raw/daemon-state.json`.
   *  With no address it is the file the caller names, read as it is, with that file's time. */
  async daemonState(daemonUrl: string | undefined, file: string | undefined): Promise<unknown> {
    const name = "daemon-state.json";
    const kept = join(this.raw, name);
    if (daemonUrl !== undefined && (this.refresh || !(await exists(kept)))) {
      await this.pace();
      const response = await fetch(`${daemonUrl.replace(/\/$/, "")}/legion/v1/state`);
      if (!response.ok)
        throw new Error(`daemon state: ${response.status} ${await response.text()}`);
      const body = await response.json();
      await this.keep(name, body);
      return body;
    }
    if (await exists(kept)) return readJson(kept);
    if (file === undefined) throw new Error("name --daemon-state <file> or --daemon-url <url>");
    const body = await readJson(file);
    this.times[name] = (await stat(file)).mtime.toISOString();
    await writeFile(kept, JSON.stringify(body));
    await writeFile(this.timesFile, JSON.stringify(this.times, null, 2));
    return body;
  }

  /** Every event of one issue, oldest first, through `lastSeq`; kept as `raw/events/<key>.json`
   *  and read again only when the listing's `last_seq` has moved past what is kept. */
  async events(key: string, lastSeq: number): Promise<ReadEvent[]> {
    const name = join("events", `${key}.json`);
    const file = join(this.raw, name);
    const kept = (await exists(file)) ? await readJson<ReadEvent[]>(file) : [];
    const keptSeq = kept.length === 0 ? 0 : kept[kept.length - 1].seq;
    if (keptSeq >= lastSeq && kept.length > 0) return kept;
    const read = [...kept];
    let after = keptSeq;
    for (;;) {
      const page = await this.get<RawEvent[]>(
        `/api/v1/issues/${encodeURIComponent(key)}/events?limit=${EVENT_PAGE}&after=${after}`
      );
      for (const event of page) read.push(reduceEvent(event));
      if (page.length < EVENT_PAGE) break;
      after = page[page.length - 1].seq;
    }
    await this.keep(name, read);
    return read;
  }

  private async keep(name: string, body: unknown): Promise<void> {
    const file = join(this.raw, name);
    await mkdir(dirname(file), { recursive: true });
    await writeFile(file, JSON.stringify(body));
    this.times[name] = new Date().toISOString();
    await writeFile(this.timesFile, JSON.stringify(this.times, null, 2));
  }

  private async pace(): Promise<void> {
    const wait = this.lastStart + READ_SPACING_MS - Date.now();
    if (wait > 0) await Bun.sleep(wait);
    this.lastStart = Date.now();
  }

  private async get<T>(path: string): Promise<T> {
    if (this.server === undefined || this.tokenFile === undefined) {
      throw new Error(`set DISPATCH_URL and DISPATCH_TOKEN_FILE to read ${path}`);
    }
    const token = (await readFile(this.tokenFile, "utf8")).trim();
    for (let attempt = 0; ; attempt++) {
      await this.pace();
      const response = await fetch(`${this.server.replace(/\/$/, "")}${path}`, {
        headers: { Authorization: `Bearer ${token}` },
      });
      if (response.ok) return (await response.json()) as T;
      if (response.status < 500 || attempt >= 3) {
        throw new Error(`GET ${path}: ${response.status} ${await response.text()}`);
      }
      await Bun.sleep(2_000 * (attempt + 1));
    }
  }
}

interface RawEvent {
  seq: number;
  type: string;
  created_at: string;
  actor?: { kind?: string; id?: string; origin?: { session_title?: string | null } | null };
  payload?: Record<string, unknown> | null;
}

function reduceEvent(event: RawEvent): ReadEvent {
  const payload = event.payload ?? {};
  const isIssue =
    event.type === "issue.updated" ||
    event.type === "issue.created" ||
    event.type === "issue.closed";
  return {
    seq: event.seq,
    type: event.type,
    created_at: event.created_at,
    actor_kind: event.actor?.kind ?? "",
    actor_id: event.actor?.id ?? "",
    session_title: event.actor?.origin?.session_title ?? null,
    status: isIssue && typeof payload.status === "string" ? payload.status : null,
    changed: isIssue && Array.isArray(payload.changed) ? (payload.changed as string[]) : null,
    handoff_phase:
      event.type === "handoff.recorded" && typeof payload.phase === "string" ? payload.phase : null,
  };
}

export async function readJson<T = unknown>(file: string): Promise<T> {
  return JSON.parse(await readFile(file, "utf8")) as T;
}

async function exists(file: string): Promise<boolean> {
  return stat(file).then(
    () => true,
    () => false
  );
}

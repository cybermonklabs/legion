/**
 * The dashboard's landing waterfall, measured on four cold pages (INFRA-1109, R3).
 *
 * Why this row exists. Opening a page of Dispatch today costs a chain of dependent round trips
 * before anything the reader asked for is drawn: `AuthGate` (`web/src/app.tsx`) holds every read
 * until `/auth/whoami` answers; the shell's reads and the lazy chunk of the page then start
 * together; the page's own first reads wait for that chunk; the reads that name what those
 * answered wait for them; and a last set waits again. A person pays that chain on every page
 * they open, and so does every browser row of this suite, which is why it costs twice - once as
 * a slow page and once as runner minutes. Nothing in the suite measured it, so nothing stopped
 * it growing. This row is that measurement. How long the chain is on the day you read this is in
 * the run's own output and not in this comment: the row prints every landing's whole waterfall,
 * passing or failing, so the work that flattens the chain has a red run to turn green and a
 * later change cannot quietly deepen it again.
 *
 * What a READ is. A GET whose path begins `/api/` or `/auth/`, and nothing else. A write (POST,
 * PUT, PATCH, DELETE) is no read, because it is not what a landing waits for; the document, the
 * scripts, the styles, the fonts and the pictures are no reads either, because they are the
 * build's cost and not the server's answer about this page. The live stream is not a read and
 * cannot be counted here even by accident: it is a WebSocket upgrade to `/ws/live`
 * (`web/src/api/live-socket.ts:207-208`), which is no GET of those two prefixes, and Playwright
 * reports it as a websocket rather than as a request.
 *
 * What a WAVE is. R3's words are "a group of reads of which none starts after the first response
 * of the next group arrives". Read over the recorded times that is one grouping and only one:
 * reads that were all in flight together - every one of them started before any of them was
 * answered - are one wave, and the next wave opens at the first read that starts once the
 * current wave's earliest response has come back, because that read could have been waiting for
 * it. `wavesOf` below is that rule and nothing else, written as a pure function over the
 * (start, response) pairs so that a reader can check it against the printed waterfall by hand.
 * A page drawn from one wave asks for everything at once; a page drawn from five asks four
 * questions whose answers it already had the means to ask for.
 *
 * What the row drives. Four cold landings - an issue page, the board, the inbox and a document
 * page - each in a browser context of its own, so no query cache, no HTTP cache and no warmed
 * chunk of an earlier landing can answer a read this one should have sent. Each landing is
 * measured from its own navigation until that page's OWN content is on the screen. "On the
 * screen" has no observable of its own, so each page names the locator the suite already waits
 * for when it lands there, and each comment beside a locator names the row it was taken from, so
 * that the two cannot drift apart: a locator that stopped meaning "this page is drawn" would
 * otherwise leave this row measuring a window that ends somewhere else.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */
import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createIssue, createProject, createProjectDocument } from "./api";
import { documentEditor } from "./editor";
import { inboxPane } from "./inbox-pane";
import { seedCommentThread, THREAD_SESSION } from "./inbox-threads";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/** R3's bound on one issue landing: at most this many reads, in at most `WAVE_BOUND` waves. */
const ISSUE_READ_BOUND = 12;
const WAVE_BOUND = 2;

/** R3's bound on every one of the four pages: the reads the shell sends before the page's own
 *  data can start are one wave, never a chain of them. */
const SHELL_WAVE_BOUND = 1;

/** How long a read that was still in flight when the page's content appeared is given to come
 *  back, so the printed waterfall says when it was answered rather than leaving a gap. It changes
 *  no wave: every read of the window started before the window closed, so a response that arrives
 *  after that can have caused none of them. */
const ANSWER_SETTLE_MS = 2_000;

/** The pages, in the order this row drives them. The assertion names the same four, so a landing
 *  dropped from here fails the row rather than quietly shrinking what "every page" covers. */
const PAGES = ["issue page", "board", "inbox", "document page"] as const;

test.beforeEach(async () => {
  await resetDatabase();
});

/**
 * One read of one landing: a GET under `/api/` or `/auth/`, with the time it started and the time
 * its response arrived, both in milliseconds from the navigation that began the landing.
 *
 * The two times are `performance.now()` readings taken as Playwright hands over the request and
 * the response, and they are fractions rather than whole milliseconds on purpose. Two events of
 * one landing can fall in the same millisecond - a cached answer, or a read the browser sends the
 * instant one lands - and read at whole-millisecond resolution a response and a request that did
 * not wait for it compare equal, which the grouping below would read as the start of a new wave.
 * The clock is monotonic and the events arrive in the order the browser raised them, so the
 * fractions keep that order and a wave is cut only where the browser really did wait.
 *
 * `respondedAt` is the one field written after the fact, by the response listener; it stands at
 * `Number.POSITIVE_INFINITY` while no response has come back, which is the honest reading for the
 * grouping - a read that was never answered inside the window started nothing inside it.
 */
interface Read {
  readonly pathname: string;
  readonly search: string;
  readonly startedAt: number;
  respondedAt: number;
}

/** What one landing cost: its reads in start order, and those reads grouped into waves. */
interface Measurement {
  readonly landing: Landing;
  readonly reads: readonly Read[];
  readonly waves: readonly (readonly Read[])[];
}

/** One page this row lands on cold. */
interface Landing {
  /** What the report calls it; one of `PAGES`. */
  readonly name: (typeof PAGES)[number];
  /** The address the cold navigation opens. */
  readonly address: string;
  /** This page's own content, which is what the measurement window ends at. */
  readonly content: (page: Page) => Locator;
  /** Whether `read` is this page's own data, as against the shell's. The reads before the first
   *  such read are the ones the shell makes before the page can even ask for what it shows. */
  readonly ownData: (read: Read) => boolean;
  /** R3's bound on the whole landing, where it sets one. It sets one for the issue page alone. */
  readonly bounds?: { readonly reads: number; readonly waves: number };
}

/** What the seed left behind for the four landings to open. */
interface World {
  readonly issue: string;
  /** The slug of the project document the document landing opens. It is a document of the
   *  project and not of the issue, because `/issues/:key/*` is the issue page's own route: an
   *  issue artifact drawn there is the issue page again with another document in it, while
   *  `/projects/:key/documents/:slug` is `DocumentPage`'s own chunk and its own read chain. */
  readonly document: string;
}

/**
 * Groups `reads` into waves, which is R3's definition and nothing more.
 *
 * Walking the reads in start order, a wave collects reads while each of them started before the
 * earliest response the wave has seen so far; the first read that starts at or after that
 * response opens the next wave, since it is the first one that could have been sent because of an
 * answer. The earliest response of a wave only ever falls as the wave grows, and a read is only
 * ever added while it starts before the earliest response so far, so every read of a wave did
 * start before the wave's own first response - which is the property R3 states.
 *
 * It is pure, and it is the only place the definition lives, so a reader can take the waterfall
 * this row prints and redo the grouping with a pencil.
 */
function wavesOf(reads: readonly Read[]): Read[][] {
  const ordered = [...reads].sort((left, right) => left.startedAt - right.startedAt);
  const waves: Read[][] = [];
  let wave: Read[] = [];
  let firstAnswer = Number.POSITIVE_INFINITY;
  for (const read of ordered) {
    if (wave.length > 0 && read.startedAt >= firstAnswer) {
      waves.push(wave);
      wave = [];
      firstAnswer = Number.POSITIVE_INFINITY;
    }
    wave.push(read);
    firstAnswer = Math.min(firstAnswer, read.respondedAt);
  }
  if (wave.length > 0) waves.push(wave);
  return waves;
}

/**
 * How many waves the shell spends before this page's own data can start: the waves that are over
 * and done before the wave the page's first own-data read stands in.
 *
 * A read of the shell's that went out in the same wave as the page's own data held nothing up -
 * the page could ask for what it shows at that moment, and did - so it is not a read made
 * "before that page's own data can start" and is not counted. Counting it would also make the
 * bound unreachable: a landing that asks for everything it needs in one wave after the viewer is
 * known would still be read as two, and whether it passed would turn on which read of that wave
 * the browser happened to send first.
 *
 * `undefined` where the landing read no data of its own at all, which is no measurement: it is
 * reported as a fault of its own rather than passing as zero.
 */
function shellWavesBeforeOwnData({ landing, reads, waves }: Measurement): number | undefined {
  const own = reads.find((read) => landing.ownData(read));
  if (own === undefined) return undefined;
  return waves.findIndex((wave) => wave.includes(own));
}

/** Collects every read `page` sends from now on, stamped against `from` (a `performance.now()`
 *  reading taken just before the navigation). */
function recordReads(page: Page, from: number): Read[] {
  const reads: Read[] = [];
  const pending = new Map<unknown, Read>();
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() !== "GET") return;
    if (!url.pathname.startsWith("/api/") && !url.pathname.startsWith("/auth/")) return;
    const read: Read = {
      pathname: url.pathname,
      respondedAt: Number.POSITIVE_INFINITY,
      search: url.search,
      startedAt: performance.now() - from,
    };
    reads.push(read);
    pending.set(request, read);
  });
  page.on("response", (response) => {
    const read = pending.get(response.request());
    if (read === undefined) return;
    read.respondedAt = performance.now() - from;
    pending.delete(response.request());
  });
  return reads;
}

/** A time as the waterfall prints it: tenths of a millisecond, right-aligned, so a reader reads
 *  the column as a column and can still see which of two reads in one millisecond came first. */
function stamp(ms: number): string {
  return Number.isFinite(ms) ? `${ms.toFixed(1).padStart(7)} ms` : "     no answer";
}

/** One landing's whole waterfall: a heading, then every read in start order with its wave. */
function waterfallOf({ landing, reads, waves }: Measurement): string {
  const lines = [
    `${landing.name} (${landing.address}): ${reads.length} reads in ${waves.length} wave(s)`,
  ];
  for (const [index, wave] of waves.entries()) {
    for (const read of wave) {
      lines.push(
        `  wave ${index + 1}  started ${stamp(read.startedAt)}  answered ${stamp(
          read.respondedAt
        )}  GET ${read.pathname}${read.search}`
      );
    }
  }
  return lines.join("\n");
}

/**
 * Drives one cold landing and measures it.
 *
 * The context is this landing's own: a context signed in afresh holds no query cache, no HTTP
 * cache and no parsed chunk of a page an earlier landing opened, which is what makes the landing
 * cold. The window runs from the navigation until the page's own content is visible; a read that
 * started inside it is counted whether or not its answer had come back by then.
 */
async function land(browser: Browser, landing: Landing): Promise<Measurement> {
  const context = await asUser(browser, "alice");
  try {
    const tab = await context.newPage();
    const from = performance.now();
    const reads = recordReads(tab, from);
    await tab.goto(landing.address);
    await expect(landing.content(tab), `${landing.name} never drew its own content`).toBeVisible();
    const until = performance.now() - from;
    // The reads of the window are these objects and no others from here on: a read that starts
    // after the window closed is not one of them, and a response still to come is written into
    // the object this already holds.
    const measured = reads
      .filter((read) => read.startedAt <= until)
      .sort((left, right) => left.startedAt - right.startedAt);
    const deadline = performance.now() + ANSWER_SETTLE_MS;
    while (
      performance.now() < deadline &&
      measured.some((read) => !Number.isFinite(read.respondedAt))
    ) {
      // Polling a value no assertion can wait on: these reads are the ones this row measures,
      // and nothing on the page changes when one of them answers, so there is no state to make
      // a web-first assertion against. 50 ms is a fortieth of the settle window above, which
      // leaves the waterfall's answer times read to the tenth of a millisecond they are printed
      // at while costing the row at most that much past the last answer.
      await tab.waitForTimeout(50);
    }
    return { landing, reads: measured, waves: wavesOf(measured) };
  } finally {
    await context.close();
  }
}

/** The world the four landings open: one project with a document of its own, one issue with a
 *  spec, and one comment thread, so the inbox has a thread card to draw. */
async function seedWorld(): Promise<World> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The landing this row measures.\n",
    title: "Measure the landing waterfall",
  });
  const rollout = await createProjectDocument("CORE", {
    content: "# Rollout\n\nHow the landing is cut to two waves.\n",
    name: "rollout.md",
  });
  await seedCommentThread(
    issue.key,
    "How many waves does a cold landing take?",
    "Whatever the run prints: this row measures it rather than guessing."
  );
  return { document: rollout.artifact.slug, issue: issue.key };
}

/** The landing route, which answers one page's whole cold read: the viewer, the shell, and the
 *  part of the answer the page named by `?page=` draws (`web/src/api/queries.ts`). */
const LANDING_PATH = "/api/v1/landing";

/** Whether `read` is the landing read that carries `page`'s own data.
 *
 *  A page's own data is whatever answers what that page shows, whichever route answered it. It
 *  used to be one per-resource read the page gated on; the landing carries those same rows now,
 *  so a landing asked for this page is that page's own data as much as the read it stands in
 *  for. Each `ownData` below accepts either, so this row measures the waves before a page can
 *  draw whether its rows came from the one route or from the many.
 */
function landingFor(read: Read, page: "issue" | "board" | "inbox" | "document"): boolean {
  return read.pathname === LANDING_PATH && new URLSearchParams(read.search).get("page") === page;
}

function landingsOf(world: World): Landing[] {
  return [
    {
      address: `/issues/${world.issue}`,
      // The issue page's own content is the first block of its primary document: the Spec tab is
      // what `/issues/<key>` opens on. The locator is `e2e/prefix-links.e2e.ts`'s, which waits for
      // `documentEditor(page).locator('[data-block-id="…"]')` when it lands on an issue's spec.
      content: (page) => documentEditor(page).locator("[data-block-id]").first(),
      bounds: { reads: ISSUE_READ_BOUND, waves: WAVE_BOUND },
      name: "issue page",
      ownData: (read) =>
        read.pathname === `/api/v1/issues/${world.issue}` || landingFor(read, "issue"),
    },
    {
      address: "/projects/CORE/issues",
      // The board's own content is the first row of the issues table. The locator is
      // `e2e/issues-table.e2e.ts`'s, which lands on `/projects/CORE/issues` and reads
      // `page.getByTestId("issues-table-scroll").locator("[data-issue-row]")`.
      content: (page) =>
        page.getByTestId("issues-table-scroll").locator("[data-issue-row]").first(),
      name: "board",
      ownData: (read) =>
        (read.pathname === "/api/v1/issues" &&
          new URLSearchParams(read.search).get("project") === "CORE") ||
        landingFor(read, "board"),
    },
    {
      address: "/",
      // The inbox's own content is the first thread card. The locator is
      // `e2e/inbox-threads.e2e.ts`'s, which lands on `/` and reads
      // `inboxPane(page).locator("[data-inbox-thread-body]")` at the reading column's width - the
      // width every project that runs this row has, and where the Inbox selects its first row as
      // it opens, so the card is drawn by the landing itself and by no click of this row's.
      content: (page) => inboxPane(page).locator("[data-inbox-thread-body]").first(),
      name: "inbox",
      ownData: (read) => read.pathname === "/api/v1/inbox" || landingFor(read, "inbox"),
    },
    {
      address: `/projects/CORE/documents/${world.document}`,
      // The project's own document page, which is `DocumentPage`'s route and chunk
      // (`web/src/app.tsx`); an issue artifact at `/issues/<key>/artifacts/<slug>` is the issue
      // page's route again and would measure that page twice. The address is
      // `e2e/document.e2e.ts`'s, which lands on `/projects/CORE/documents/<slug>` and waits for
      // `documentEditor(page)`; the block locator inside it is `e2e/prefix-links.e2e.ts`'s, as
      // on the issue page above.
      content: (page) => documentEditor(page).locator("[data-block-id]").first(),
      name: "document page",
      // The read this page gates on: `DocumentPage` holds everything else until
      // `["artifact-ref", "CORE/<slug>"]` answers with the document's id
      // (`web/src/features/document/DocumentPage.tsx`, `web/src/api/useProjectArtifact.ts`), and
      // the subscribers, text, blocks and asks reads all name that id.
      ownData: (read) =>
        read.pathname === `/api/v1/projects/CORE/artifacts/${world.document}` ||
        landingFor(read, "document"),
    },
  ];
}

// SLOW(>=2s): four cold landings, each in a context of its own - a fresh Chromium context, the
// cold parse of the dashboard bundle it cannot cache, and the landing's own reads, four times
// over. The cost is the measurement: a warm context would answer reads from its query cache and
// count waves that a person opening the page never pays.
// @pr: The dashboard's landing waterfall, which every browser row of this suite pays.
test("a cold landing draws each page from at most two waves of reads @pr", async ({ browser }) => {
  const world = await seedWorld();

  const measured: Measurement[] = [];
  for (const landing of landingsOf(world)) {
    measured.push(await land(browser, landing));
  }

  const waterfall = measured.map(waterfallOf).join("\n\n");
  console.log(`LANDING-READS\n${waterfall}`);

  // The count of pages driven is asserted first, and by name: "on each of the four pages" is true
  // of no pages at all, so a landing that was dropped or never ran has to fail the row rather
  // than make the bounds below vacuous.
  expect(
    measured.map(({ landing }) => landing.name),
    "the row drives four cold landings"
  ).toEqual([...PAGES]);

  const faults: string[] = [];
  for (const measurement of measured) {
    const { landing, reads, waves } = measurement;
    if (landing.bounds !== undefined) {
      if (reads.length > landing.bounds.reads) {
        faults.push(
          `the ${landing.name} sent ${reads.length} reads, over the ${landing.bounds.reads} ` +
            "R3 allows"
        );
      }
      if (waves.length > landing.bounds.waves) {
        faults.push(
          `the ${landing.name} took ${waves.length} waves, over the ${landing.bounds.waves} ` +
            "R3 allows"
        );
      }
    }
    const shell = shellWavesBeforeOwnData(measurement);
    if (shell === undefined) {
      faults.push(`the ${landing.name} read no data of its own, so its shell waves cannot be read`);
    } else if (shell > SHELL_WAVE_BOUND) {
      faults.push(
        `the ${landing.name} spent ${shell} waves on shell reads before its own data could ` +
          `start, over the ${SHELL_WAVE_BOUND} R3 allows`
      );
    }
  }

  expect(
    faults.join("\n"),
    `the landing waterfall of each page, in start order:\n\n${waterfall}`
  ).toBe("");
});

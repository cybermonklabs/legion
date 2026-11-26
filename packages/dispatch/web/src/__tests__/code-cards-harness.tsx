// The cards' state table of dispatch://INFRA-3206 (plan, "The cards' state table (UI with
// timing)"): each row one test over one of the two code cards of the Evals tab, driven as the
// tab's own state rows are (EvalsTab.test.tsx). A fake live socket answers the tab's delivery view
// and the card's items view in the order the row names, and the project's delivery settings hold
// its code groups. CodeSurvivalCard.test.tsx and PostMergeReworkCard.test.tsx each run every row
// over their own card.
import { expect, type Mock, spyOn } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type {
  DeliveryLiveDelta,
  EvalDetailResponse,
  EvalItemRow,
  EvalListItem,
  EvalListResponse,
  LiveSubscribe,
  ProjectDeliverySettings,
} from "../api/types";
import type { EvalRange, EvalSeriesValues } from "../features/evals/chart-model";
import { EvalsTab } from "../features/evals/EvalsTab";
import { deliverySettings } from "./delivery-settings";
import { evalItem, evalSeries } from "./evals-fixture";
import {
  type FakeLiveSocket,
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "./live-socket-harness";

/** One code card as its rows read it: its eval, its title, the base of each group's headline
 *  series (`survival:<g>` or `rework:<g>`), and the bases of the group series it draws beside
 *  the headline (the rework card's other kinds of removal). */
export interface CodeCardCase {
  readonly slug: string;
  readonly title: string;
  readonly base: string;
  readonly kinds: readonly string[];
}

export const SURVIVAL_CARD: CodeCardCase = {
  base: "survival",
  kinds: [],
  slug: "code-survival-30d",
  title: "Code survival at 30 days",
};

export const REWORK_CARD: CodeCardCase = {
  base: "rework",
  kinds: ["iteration", "noissue", "other"],
  slug: "post-merge-rework-30d",
  title: "Post-merge rework at 30 days",
};

/** The window of the tab's 28-day preset as the view answers it: 28 whole UTC days, long past. */
export const WINDOW: EvalRange = {
  days: 28,
  from: "2026-10-01T00:00:00Z",
  to: "2026-10-29T00:00:00Z",
};

const COMPARE: EvalRange = {
  days: 28,
  from: "2026-09-03T00:00:00Z",
  to: "2026-10-01T00:00:00Z",
};

/** The 7-day and 84-day windows the header's presets name. */
const WINDOW_7: EvalRange = { days: 7, from: "2026-10-22T00:00:00Z", to: "2026-10-29T00:00:00Z" };
const WINDOW_84: EvalRange = {
  days: 84,
  from: "2026-08-06T00:00:00Z",
  to: "2026-10-29T00:00:00Z",
};

/** The first day the eval's series hold a row of: the window's first day. */
const FIRST_DAY = "2026-10-01";

/** One group's figures over a window, as the eval's fold states them (`share`, `lines`,
 *  `added`, `changes`, `via_repository`), every one null where the window reads "no data". */
export interface GroupFigures {
  readonly name: string;
  readonly share: number | null;
  readonly lines: number | null;
  readonly added: number | null;
  readonly changes: number | null;
  readonly via: number | null;
}

/** The status series' figures over a window (`fold.FoldStatus`); null for a window the server
 *  answers no figure of (before the first day, or no row at all). */
export interface StatusFigures {
  readonly measured: number;
  readonly marked?: number;
  readonly blocked?: number;
  readonly blockedFiles?: number;
  /** The oldest marked day, YYYY-MM-DD. */
  readonly firstMarked?: string;
  readonly repoUnreadable?: number;
  readonly waiting: number;
}

export const AGENTS: GroupFigures = {
  added: 150,
  changes: 2,
  lines: 105,
  name: "agents",
  share: 0.7,
  via: 1,
};

export const PEOPLE: GroupFigures = {
  added: 40,
  changes: 1,
  lines: 20,
  name: "people",
  share: 0.5,
  via: 1,
};

/** A group's figures over a window that reads "no data". */
function blank(name: string): GroupFigures {
  return { added: null, changes: null, lines: null, name, share: null, via: null };
}

/** The project's two code groups, in order: agents by head branch, and every other change. */
const GROUPS: ProjectDeliverySettings["code_groups"] = [
  { author_logins: [], branch_prefixes: ["agent/"], name: "agents" },
  { author_logins: [], branch_prefixes: [], name: "people" },
];

/** ACME's delivery settings with its two code groups, as `overrides` changes them. */
export function codeSettings(
  overrides: Partial<ProjectDeliverySettings> = {}
): ProjectDeliverySettings {
  return deliverySettings("ACME", { code_groups: GROUPS, ...overrides });
}

/** Each kind's share beside a group's rework share, on the rework card. */
const KIND_SHARES: Readonly<Record<string, number>> = {
  iteration: 0.02,
  noissue: 0.005,
  other: 0.03,
};

/** `card`'s eval as the delivery view answers it over a window: the status series and, for each
 *  of `groups`, its headline series and its kinds, every series first holding a row on
 *  `dataSince` (null: no row at all), at `version`. `status` null answers no status figure. */
export function codeEval(
  card: CodeCardCase,
  groups: readonly GroupFigures[],
  status: StatusFigures | null,
  dataSince: string | null = FIRST_DAY,
  version = 1
): EvalListItem {
  const statusSeries = evalSeries("status", "days", {
    agg: "sum",
    data_since: dataSince,
    eval_id: `ACME/${card.slug}`,
    role: "control",
    window:
      status === null
        ? {
            blocked_days: null,
            blocked_files: null,
            first_marked_day: null,
            groups: null,
            marked_days: null,
            measured_days: null,
            repo_unreadable: null,
            waiting_days: null,
          }
        : {
            blocked_days: status.blocked ?? 0,
            blocked_files: status.blockedFiles ?? 0,
            first_marked_day:
              status.firstMarked === undefined
                ? null
                : Date.parse(`${status.firstMarked}T00:00:00Z`) / 1000,
            groups: groups.length,
            marked_days: status.marked ?? 0,
            measured_days: status.measured,
            repo_unreadable: status.repoUnreadable ?? 0,
            waiting_days: status.waiting,
          },
  });
  const groupSeries = groups.flatMap((group): EvalSeriesValues[] => [
    evalSeries(`${card.base}:${group.name}`, "ratio", {
      agg: "ratio",
      data_since: dataSince,
      eval_id: `ACME/${card.slug}`,
      window: {
        added: group.added,
        changes: group.changes,
        lines: group.lines,
        share: group.share,
        via_repository: group.via,
      },
    }),
    ...card.kinds.map((kind) =>
      evalSeries(`${kind}:${group.name}`, "ratio", {
        agg: "ratio",
        data_since: dataSince,
        eval_id: `ACME/${card.slug}`,
        role: "control",
        window: {
          added: group.added,
          changes: group.changes,
          lines: group.share === null ? null : 1,
          share: group.share === null ? null : (KIND_SHARES[kind] ?? null),
          via_repository: group.via,
        },
      })
    ),
  ]);
  const item = evalItem(card.slug, "delivery", [statusSeries, ...groupSeries], version);
  return { ...item, definition: { ...item.definition, name: card.title, unit: "ratio" } };
}

/** A delivery view's answer over `window`, holding `items`. */
function answer(items: EvalListItem[], window: EvalRange = WINDOW): EvalListResponse {
  return {
    compare: COMPARE,
    delivery_freshness: null,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items,
    limit: 200,
    offset: 0,
    total: items.length,
    window,
  };
}

/** A delta of the view `sub` that changes `upsert`. */
function deltaFrame(sub: string, upsert: EvalListItem[], seq: string): DeliveryLiveDelta {
  return {
    changed_at: null,
    counts: {},
    items: { items: { remove: [], upsert } },
    maps: {},
    revs: {
      items: {
        items: Object.fromEntries(upsert.map((item) => [item.id, `00000000000000${seq}a`])),
      },
    },
    seq,
    set: {},
    sub,
    type: "delta",
  };
}

/** The method text each eval's detail read answers. */
export const METHOD =
  "**What the number is not.**\n\n- It is the replay's reading, not blame.\n- The diff is GitHub's, with three lines of context.";

/** An eval's detail read as Dispatch answers it: its method and no declared chart. */
function detailOf(docText: string): EvalDetailResponse {
  return {
    compare: COMPARE,
    doc_text: docText,
    freshness: { refresher_age_seconds: null, rows_updated_at: null },
    items: [],
    limit: 1,
    offset: 0,
    total: 1,
    window: WINDOW,
  };
}

/** What a row holds of the tab it opened: its live socket, and the detail read's spy. */
interface OpenTab {
  readonly socket: FakeLiveSocket;
  readonly sockets: FakeLiveSockets;
  readonly getEval: Mock<typeof api.getEval>;
}

/** The tab of ACME (no Legion daemon, delivery on, `settings`), at the 28-day preset, with its
 *  live socket open. */
async function openTab(
  sockets: FakeLiveSockets,
  spies: { mockRestore: () => void }[],
  settings: ProjectDeliverySettings
): Promise<OpenTab> {
  const daemon = spyOn(api, "getLegionDaemon").mockResolvedValue(null);
  const read = spyOn(api, "getProjectDeliverySettings").mockResolvedValue(settings);
  const getEval = spyOn(api, "getEval").mockResolvedValue(detailOf(METHOD));
  spies.push(daemon, read, getEval);
  render(
    <MemoryRouter initialEntries={["/projects/ACME/evals?window=28d"]}>
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <EvalsTab project="ACME" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  return { getEval, socket, sockets };
}

/** The newest subscribe frame of the delivery view whose window is `preset` days, once sent. */
function deliveryView(socket: FakeLiveSocket, preset = 28): Promise<LiveSubscribe> {
  return waitFor(() => {
    const frame = socket
      .subscribes()
      .filter(
        (sent) =>
          sent.view.kind === "evals" &&
          new URLSearchParams(sent.view.query).get("kind") === "delivery" &&
          "preset" in sent.view.window &&
          sent.view.window.preset === preset
      )
      .at(-1);
    if (frame === undefined) throw new Error(`no ${preset}-day delivery view was subscribed`);
    return frame;
  });
}

/** Every subscribe frame of `slug`'s items view of its absent rows. */
function absentViews(socket: FakeLiveSocket, slug: string): LiveSubscribe[] {
  return socket
    .subscribes()
    .filter(
      (sent) =>
        sent.view.kind === "items" &&
        sent.view.slug === slug &&
        new URLSearchParams(sent.view.query).get("row_kind") === "absent"
    );
}

/** The absent rows' view of `slug`, once the card has subscribed it. */
function absentView(socket: FakeLiveSocket, slug: string): Promise<LiveSubscribe> {
  return waitFor(() => {
    const frame = absentViews(socket, slug).at(-1);
    if (frame === undefined) throw new Error(`no absent rows of ${slug} were subscribed`);
    return frame;
  });
}

/** Dispatch sends `frame` to the page. */
function serve(socket: FakeLiveSocket, frame: Parameters<FakeLiveSocket["receive"]>[0]): void {
  act(() => socket.receive(frame));
}

/** One absent row of an eval, as the items route answers it. */
function absentRow(
  key: string,
  cols: Record<string, unknown>,
  href: string,
  links: EvalItemRow["links"] = []
): EvalItemRow {
  return {
    cols,
    href,
    links,
    measured_at: "2026-10-05T00:00:00Z",
    range: "window",
    row_key: key,
    row_kind: "absent",
    source: "code_days",
  };
}

/** The commit that blocks three days of the window, and its address on GitHub. */
const BLOCKED_SHA = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d";
const BLOCKED_HREF = `https://github.com/acme/widgets/commit/${BLOCKED_SHA}`;

const BLOCKED_ROW = absentRow(
  `blocked@acme/widgets@${BLOCKED_SHA}`,
  {
    cause: "blocked",
    days: ["2026-10-05", "2026-10-06", "2026-10-07"],
    files: 3,
    reason: "GitHub refused the diff and withheld 3 files' patches",
    repo: "acme/widgets",
    sha: BLOCKED_SHA,
  },
  BLOCKED_HREF,
  [{ href: BLOCKED_HREF, label: "commit 1a2b3c4", recorded_at: "2026-10-08T00:00:00Z", rel: "via" }]
);

/** One page of `slug`'s absent rows. */
function absentPage(rows: EvalItemRow[]) {
  return { compare: null, items: rows, limit: 20, offset: 0, total: rows.length, window: WINDOW };
}

/** The card of `card` in Deploys and lead time: the code card itself (`data-code-card`), never
 *  the box the section draws an eval it does not name in (`UnnamedEvals`), which carries the
 *  same `data-eval`. */
export function cardOf(card: CodeCardCase): HTMLElement {
  const found = screen
    .getByRole("region", { name: "Deploys and lead time" })
    .querySelector<HTMLElement>(`[data-code-card="${card.slug}"]`);
  if (found === null) throw new Error(`no ${card.slug} card`);
  return found;
}

/** The card of `card`, once it has drawn. */
export function drawnCard(card: CodeCardCase): Promise<HTMLElement> {
  return waitFor(() => cardOf(card));
}

/** The headline share the card draws for each group, by group, in the order it draws them. */
function shares(card: CodeCardCase): [string, string | null][] {
  return [...cardOf(card).querySelectorAll("[data-group]")].map((group) => [
    group.getAttribute("data-group") ?? "",
    group.querySelector("[data-share]")?.textContent ?? null,
  ]);
}

/** What the card says of a window with no number: each "no data" line. */
function noData(card: CodeCardCase): string[] {
  return [...cardOf(card).querySelectorAll("[data-no-data]")].map((line) => line.textContent ?? "");
}

function waiting(card: CodeCardCase): string | null {
  return cardOf(card).querySelector("[data-waiting]")?.textContent ?? null;
}

function note(card: CodeCardCase): string | null {
  return cardOf(card).querySelector('[data-note="repository"]')?.textContent ?? null;
}

/** A card that reads no number draws no share and no 0 anywhere on its face. */
function expectNoNumber(card: CodeCardCase): void {
  expect(shares(card)).toEqual([]);
  expect(cardOf(card).textContent ?? "").not.toMatch(/(^|[^\d.])0(\.0)?%/);
}

/** The tab's state for one row: its fake sockets and the spies it installed. */
interface RowContext {
  readonly sockets: FakeLiveSockets;
  readonly spies: { mockRestore: () => void }[];
}

/** `body` as one row: the fake sockets installed for it and restored after it, with its page
 *  unmounted inside it rather than in an `afterEach`, as EvalsTab.test.tsx's rows do. */
function row(body: (context: RowContext) => Promise<void>): () => Promise<void> {
  return async () => {
    const context: RowContext = { sockets: installFakeLiveSockets(), spies: [] };
    try {
      await body(context);
    } finally {
      cleanup();
      context.sockets.restore();
      for (const spy of context.spies.splice(0)) spy.mockRestore();
    }
  };
}

/** The tab with `card`'s eval answered as `item`, its delivery view's sub, and the socket. */
async function painted(
  context: RowContext,
  card: CodeCardCase,
  item: EvalListItem,
  settings = codeSettings(),
  seq = "1"
): Promise<OpenTab & { sub: string }> {
  const tab = await openTab(context.sockets, context.spies, settings);
  const { sub } = await deliveryView(tab.socket);
  serve(tab.socket, snapshotFrame(sub, answer([item]), { seq }));
  await drawnCard(card);
  return { ...tab, sub };
}

/** The value state: both groups read over 28 measured days. */
function valued(card: CodeCardCase): EvalListItem {
  return codeEval(card, [AGENTS, PEOPLE], { measured: 28, waiting: 0 });
}

/** The two groups' shares as the value state draws them. */
const VALUED: [string, string | null][] = [
  ["agents", "70.0%"],
  ["people", "50.0%"],
];

/** The state table's rows over `card`, each `[name, body]`, in the table's order. */
export function codeCardRows(card: CodeCardCase): [string, () => Promise<void>][] {
  return [
    [
      "1 no answer | the view's answer with measured days | each group's share, the face names, the repository note",
      row(async (context) => {
        await painted(context, card, valued(card));
        expect(shares(card)).toEqual(VALUED);
        const face = cardOf(card).querySelector("[data-reading]")?.textContent ?? "";
        expect(face).toContain("replay reading, not blame");
        expect(face).toContain("GitHub diff");
        expect(note(card)).toBe(
          "2 changes with no issue reached this project through acme/widgets"
        );
        expect(noData(card)).toEqual([]);
        expect(waiting(card)).toBeNull();
      }),
    ],
    [
      '2 no answer | an answer with measured and waiting days | the share of the measured days and "N days still waiting"',
      row(async (context) => {
        await painted(
          context,
          card,
          codeEval(card, [AGENTS, PEOPLE], { measured: 20, waiting: 8 })
        );
        expect(shares(card)).toEqual(VALUED);
        expect(waiting(card)).toBe("8 days still waiting");
        expect(noData(card)).toEqual([]);
      }),
    ],
    [
      '3 no answer | an answer with no measured day | "No data: N days still waiting", never 0',
      row(async (context) => {
        await painted(
          context,
          card,
          codeEval(card, [blank("agents"), blank("people")], { measured: 0, waiting: 28 })
        );
        expect(noData(card)).toEqual(["No data: 28 days still waiting"]);
        expect(waiting(card)).toBeNull();
        expectNoNumber(card);
      }),
    ],
    [
      '4 no answer | the settings hold no group | "No data: name this project\'s code groups", linked to its settings',
      row(async (context) => {
        await painted(
          context,
          card,
          codeEval(card, [], { measured: 28, waiting: 0 }),
          codeSettings({ code_groups: [] })
        );
        expect(noData(card)).toEqual(["No data: name this project's code groups"]);
        expect(
          within(cardOf(card)).getByRole("link", { name: "name this project's code groups" })
        ).toHaveAttribute("href", "/projects/ACME/settings");
        expectNoNumber(card);
        expect(note(card)).toBeNull();
      }),
    ],
    [
      "5 a value | a delta moves a day row | the new share, with no reload",
      row(async (context) => {
        const { socket, sub } = await painted(context, card, valued(card));
        expect(shares(card)).toEqual(VALUED);
        const sent = socket.subscribes().length;
        serve(
          socket,
          deltaFrame(
            sub,
            [
              codeEval(card, [{ ...AGENTS, lines: 120, share: 0.8 }, PEOPLE], {
                measured: 28,
                waiting: 0,
              }),
            ],
            "2"
          )
        );
        await waitFor(() =>
          expect(shares(card)).toEqual([
            ["agents", "80.0%"],
            ["people", "50.0%"],
          ])
        );
        // The open view carried it: nothing was subscribed again, and nothing let go.
        expect(socket.subscribes()).toHaveLength(sent);
        expect(socket.unsubscribes().map((frame) => frame.sub)).not.toContain(sub);
      }),
    ],
    [
      "6 a value with waiting days | a delta measures seven days | the share moves and the waiting count falls",
      row(async (context) => {
        const { socket, sub } = await painted(
          context,
          card,
          codeEval(card, [AGENTS, PEOPLE], { measured: 18, waiting: 10 })
        );
        expect(waiting(card)).toBe("10 days still waiting");
        serve(
          socket,
          deltaFrame(
            sub,
            [
              codeEval(card, [{ ...AGENTS, added: 200, lines: 150, share: 0.75 }, PEOPLE], {
                measured: 25,
                waiting: 3,
              }),
            ],
            "2"
          )
        );
        await waitFor(() => expect(waiting(card)).toBe("3 days still waiting"));
        expect(shares(card)).toEqual([
          ["agents", "75.0%"],
          ["people", "50.0%"],
        ]);
      }),
    ],
    [
      '7 a value | a delta marks a day | "No data: rows marked after a force-push found on <day>"',
      row(async (context) => {
        const { socket, sub } = await painted(context, card, valued(card));
        serve(
          socket,
          deltaFrame(
            sub,
            [
              codeEval(card, [blank("agents"), blank("people")], {
                firstMarked: "2026-10-05",
                marked: 1,
                measured: 27,
                waiting: 0,
              }),
            ],
            "2"
          )
        );
        await waitFor(() =>
          expect(noData(card)).toEqual([
            "No data: rows marked after a force-push found on 2026-10-05",
          ])
        );
        expectNoNumber(card);
        // The status names the day: the card reads no absent row for it.
        expect(absentViews(socket, card.slug)).toEqual([]);
      }),
    ],
    [
      '8 a value | a delta blocks days | "No data" with the commit and its file count once the absent row arrives',
      row(async (context) => {
        const { socket, sub } = await painted(context, card, valued(card));
        serve(
          socket,
          deltaFrame(
            sub,
            [
              codeEval(card, [blank("agents"), blank("people")], {
                blocked: 3,
                blockedFiles: 9,
                measured: 25,
                waiting: 0,
              }),
            ],
            "2"
          )
        );
        // Until the absent row arrives: "No data", and no commit named yet.
        await waitFor(() => expect(noData(card)).toEqual(["No data"]));
        expect(within(cardOf(card)).getByRole("status").textContent).toBe("Reading why…");
        expectNoNumber(card);
        const absent = await absentView(socket, card.slug);
        expect(absent.view).toMatchObject({ kind: "items", project: "ACME", slug: card.slug });
        serve(socket, snapshotFrame(absent.sub, absentPage([BLOCKED_ROW])));
        await waitFor(() =>
          expect(noData(card)).toEqual([
            "No data: GitHub did not give the whole patch of commit 1a2b3c4 (3 files)",
          ])
        );
        expect(within(cardOf(card)).getByRole("link", { name: "1a2b3c4" })).toHaveAttribute(
          "href",
          BLOCKED_HREF
        );
        expect(within(cardOf(card)).queryByRole("status")).toBeNull();
      }),
    ],
    [
      "9 marked or blocked | a delta measures the days | the value again, and no absent read",
      row(async (context) => {
        const { socket, sub } = await painted(
          context,
          card,
          codeEval(card, [blank("agents"), blank("people")], {
            blocked: 3,
            blockedFiles: 9,
            measured: 25,
            waiting: 0,
          })
        );
        const absent = await absentView(socket, card.slug);
        serve(socket, snapshotFrame(absent.sub, absentPage([BLOCKED_ROW])));
        await waitFor(() => expect(noData(card)).toHaveLength(1));
        serve(socket, deltaFrame(sub, [valued(card)], "2"));
        await waitFor(() => expect(shares(card)).toEqual(VALUED));
        expect(noData(card)).toEqual([]);
        // The absent rows' view is let go, and none is read again.
        await waitFor(() =>
          expect(socket.unsubscribes().map((frame) => frame.sub)).toContain(absent.sub)
        );
        expect(absentViews(socket, card.slug)).toHaveLength(1);
      }),
    ],
    [
      '10 no data | the absent rows name an unreadable repository | "No data: Dispatch\'s GitHub App cannot read <repo>"',
      row(async (context) => {
        const { socket } = await painted(
          context,
          card,
          codeEval(card, [blank("agents"), blank("people")], {
            measured: 28,
            repoUnreadable: 28,
            waiting: 0,
          })
        );
        const absent = await absentView(socket, card.slug);
        serve(
          socket,
          snapshotFrame(
            absent.sub,
            absentPage([
              absentRow(
                "repo_unreadable@acme/widgets",
                { cause: "repo_unreadable", reason: "404 Not Found", repo: "acme/widgets" },
                "/projects/ACME/settings"
              ),
            ])
          )
        );
        await waitFor(() =>
          expect(noData(card)).toEqual(["No data: Dispatch's GitHub App cannot read acme/widgets"])
        );
        expectNoNumber(card);
      }),
    ],
    [
      "11 a value | the reader picks another window in the header | the new window's answer",
      row(async (context) => {
        const user = userEvent.setup();
        const { socket } = await painted(context, card, valued(card));
        await user.selectOptions(screen.getByLabelText("Window"), "7d");
        const next = await deliveryView(socket, 7);
        serve(
          socket,
          snapshotFrame(
            next.sub,
            answer(
              [
                codeEval(card, [{ ...AGENTS, share: 0.6 }, PEOPLE], {
                  measured: 7,
                  waiting: 0,
                }),
              ],
              WINDOW_7
            )
          )
        );
        await waitFor(() =>
          expect(shares(card)).toEqual([
            ["agents", "60.0%"],
            ["people", "50.0%"],
          ])
        );
      }),
    ],
    [
      "12 a value | the reader clicks the card's title | the panel with the method text word for word",
      row(async (context) => {
        const user = userEvent.setup();
        const { getEval } = await painted(context, card, valued(card));
        await user.click(within(cardOf(card)).getByRole("button", { name: card.title }));
        const panel = screen.getByRole("dialog", { name: card.title });
        await waitFor(() =>
          expect(panel.querySelector("[data-method-text]")?.textContent).toBe(METHOD)
        );
        expect(getEval.mock.calls).toContainEqual(["ACME", card.slug, { limit: 1 }]);
      }),
    ],
    [
      "13 a value | the socket reconnects and resumes | the value stands and the view resumes",
      row(async (context) => {
        const { socket, sockets, sub } = await painted(
          context,
          card,
          valued(card),
          codeSettings(),
          "5"
        );
        act(() => socket.restart());
        // Lost, and not yet back: the card keeps what it drew.
        expect(shares(card)).toEqual(VALUED);
        await waitFor(() => expect(sockets.sockets.length).toBe(2));
        const next = sockets.latest();
        act(() => next.open());
        const resumed = next.subscribes().find((frame) => frame.sub === sub);
        expect(resumed?.resume).toEqual({
          revs: { counts: {}, fields: {}, items: {}, maps: {} },
          seq: "5",
        });
        expect(shares(card)).toEqual(VALUED);
      }),
    ],
    [
      '14 a value | the window starts before the first day | "No data before <day>"',
      row(async (context) => {
        const user = userEvent.setup();
        const { socket } = await painted(context, card, valued(card));
        await user.selectOptions(screen.getByLabelText("Window"), "84d");
        const next = await deliveryView(socket, 84);
        // The window starts before the series' first day: the server states no figure of it.
        serve(
          socket,
          snapshotFrame(
            next.sub,
            answer([codeEval(card, [blank("agents"), blank("people")], null)], WINDOW_84)
          )
        );
        await waitFor(() => expect(noData(card)).toEqual(["No data before 2026-10-01"]));
        expectNoNumber(card);
        expect(waiting(card)).toBeNull();
      }),
    ],
    [
      '15 no data | the absent rows name no default repository and no change | "No data: set this project\'s default repository", linked to its settings',
      row(async (context) => {
        const { socket } = await painted(
          context,
          card,
          codeEval(card, [blank("agents"), blank("people")], null, null),
          codeSettings({ default_repo: null })
        );
        const absent = await absentView(socket, card.slug);
        serve(
          socket,
          snapshotFrame(
            absent.sub,
            absentPage([
              absentRow("no_default_repo", { cause: "no_default_repo" }, "/projects/ACME/settings"),
            ])
          )
        );
        await waitFor(() =>
          expect(noData(card)).toEqual(["No data: set this project's default repository"])
        );
        expect(
          within(cardOf(card)).getByRole("link", { name: "set this project's default repository" })
        ).toHaveAttribute("href", "/projects/ACME/settings");
        expectNoNumber(card);
      }),
    ],
  ];
}

/** Both code evals answered in the value state, beside the title-match Rework share eval, on
 *  the open tab; `body` reads it. */
export function bothCardsRow(body: () => Promise<void> | void): () => Promise<void> {
  return row(async (context) => {
    const tab = await openTab(context.sockets, context.spies, codeSettings());
    const { sub } = await deliveryView(tab.socket);
    const reworkShare = evalItem("rework-share", "delivery", [
      evalSeries("prs", "ratio", {
        days: [{ day: FIRST_DAY, statistics: { rate: 0.1 } }],
        is_primary: true,
        window: { rate: 0.1 },
      }),
    ]);
    serve(
      tab.socket,
      snapshotFrame(
        sub,
        answer([
          { ...reworkShare, definition: { ...reworkShare.definition, name: "Rework share" } },
          valued(SURVIVAL_CARD),
          valued(REWORK_CARD),
        ])
      )
    );
    await drawnCard(SURVIVAL_CARD);
    await drawnCard(REWORK_CARD);
    await body();
  });
}

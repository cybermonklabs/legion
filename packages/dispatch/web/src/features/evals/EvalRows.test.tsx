import { afterEach, beforeEach, test as bunTest, expect } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render, waitFor, within } from "@testing-library/react";

import { itemsPage } from "../../__tests__/evals-fixture";
import {
  type FakeLiveSockets,
  installFakeLiveSockets,
  snapshotFrame,
} from "../../__tests__/live-socket-harness";
import type { EvalItemRow } from "../../api/types";
import { EvalRows } from "./EvalRows";
import type { EvalsScope } from "./useEvalsViews";

// The rows behind a number (dispatch://INFRA-2918 R10), for the row kinds of the code evals of
// dispatch://INFRA-3206 (R13): Dispatch computes them, so each draws its columns and its links
// rather than reading as a value an agent logged.

const SCOPE: EvalsScope = { compare: null, project: "ACME", repos: [], window: { preset: 28 } };

const ADDER = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d";
const REMOVER = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c";
const BLOCKER = "0123456789abcdef0123456789abcdef01234567";

const commit = (sha: string) => `https://github.com/acme/widgets/commit/${sha}`;

/** A row of a code eval as the items route answers it. */
function codeRow(
  kind: string,
  key: string,
  cols: Record<string, unknown>,
  href: string,
  links: EvalItemRow["links"]
): EvalItemRow {
  return {
    cols,
    href,
    links,
    measured_at: "2026-10-02T10:00:00Z",
    range: "window",
    row_key: key,
    row_kind: kind,
    source: kind === "absent" ? "code_days" : "delivery/survival.Measure",
  };
}

const CHANGE = codeRow(
  "change",
  `acme/widgets@${ADDER}`,
  {
    added: 120,
    alive: 90,
    group: "agents",
    issue: "ACME-3",
    pr_number: 41,
    sha: ADDER,
    share: 0.75,
    via_repository: false,
  },
  "https://github.com/acme/widgets/pull/41",
  [
    {
      href: commit(ADDER),
      label: "commit 1a2b3c4",
      recorded_at: "2026-11-04T00:00:00Z",
      rel: "via",
    },
  ]
);

const PAIR = codeRow(
  "pair",
  `acme/widgets@${ADDER}>${REMOVER}`,
  {
    adder_issue: "ACME-3",
    adder_pr: 41,
    days_between: 4.5,
    group: "agents",
    kind: "rework",
    lines: 12,
    remover_issue: "ACME-5",
    remover_pr: 44,
  },
  "https://github.com/acme/widgets/pull/44",
  [
    {
      href: commit(REMOVER),
      label: "removing commit 9f8e7d6",
      recorded_at: "2026-11-04T00:00:00Z",
      rel: "via",
    },
    {
      href: commit(ADDER),
      label: "adding commit 1a2b3c4",
      recorded_at: "2026-11-04T00:00:00Z",
      rel: "related",
    },
  ]
);

const ABSENT = codeRow(
  "absent",
  `blocked@acme/widgets@${BLOCKER}`,
  {
    cause: "blocked",
    days: ["2026-10-02"],
    files: 3,
    reason: "GitHub withheld 3 files' patches",
    repo: "acme/widgets",
    sha: BLOCKER,
  },
  commit(BLOCKER),
  [
    {
      href: commit(BLOCKER),
      label: "commit 0123456",
      recorded_at: "2026-11-04T00:00:00Z",
      rel: "via",
    },
  ]
);

let sockets: FakeLiveSockets;
beforeEach(() => {
  sockets = installFakeLiveSockets();
});
afterEach(() => {
  sockets.restore();
});

const test = (name: string, body: () => Promise<void>): void =>
  bunTest(name, async () => {
    try {
      await body();
    } finally {
      cleanup();
    }
  });

test("a change row, a pair row and an absent row each draw their columns and links", async () => {
  render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <EvalRows label="Rows" limit={50} scope={SCOPE} slug="code-survival-30d" />
    </QueryClientProvider>
  );
  await waitFor(() => expect(sockets.sockets.length).toBeGreaterThan(0));
  const socket = sockets.latest();
  act(() => socket.open());
  const items = await waitFor(() => socket.subscribed("items"));
  act(() => socket.receive(snapshotFrame(items.sub, itemsPage([CHANGE, PAIR, ABSENT]))));

  const drawn = (row: EvalItemRow) =>
    waitFor(() => {
      const found = document.querySelector<HTMLElement>(
        `[data-eval-row="${CSS.escape(row.row_key)}"]`
      );
      if (found === null) throw new Error(`no row ${row.row_key}`);
      return found;
    });
  const expected: [EvalItemRow, string[], [string, string][]][] = [
    [
      CHANGE,
      ["group agents", "added 120", "alive 90", "share 0.75", "pr_number 41", "issue ACME-3"],
      [["via", commit(ADDER)]],
    ],
    [
      PAIR,
      ["kind rework", "lines 12", "days_between 4.5", "adder_issue ACME-3", "remover_issue ACME-5"],
      [
        ["via", commit(REMOVER)],
        ["related", commit(ADDER)],
      ],
    ],
    [ABSENT, ["cause blocked", "files 3", "repo acme/widgets"], [["via", commit(BLOCKER)]]],
  ];
  for (const [row, columns, links] of expected) {
    const element = await drawn(row);
    // A row Dispatch computes, not a reading as an agent logged it.
    expect(element).not.toHaveAttribute("data-reading");
    expect(element.textContent).not.toContain("not measured");
    for (const column of columns) expect(element.textContent).toContain(column);
    expect(element.querySelector("[data-row-href]")).toHaveAttribute("href", row.href);
    expect(
      within(element)
        .getByRole("list", { name: `Links of ${row.row_key}` })
        .querySelectorAll("a")
    ).toHaveLength(links.length);
    for (const [rel, href] of links) {
      expect(element.querySelector(`a[data-rel="${rel}"]`)).toHaveAttribute("href", href);
    }
  }
});

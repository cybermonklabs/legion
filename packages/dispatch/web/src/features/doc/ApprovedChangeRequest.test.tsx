import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { Artifact, ArtifactApproval, ChangeRequestRead, SpecNodeRead } from "../../api/types";
import { ApprovedChangeRequest } from "./ApprovedChangeRequest";
import {
  approvedNode,
  changeRequest,
  ISSUE,
  noSourceRequest,
  requestNode,
} from "./change-request-fixture";

// R22: "After the approval, the issue shows the change request as the person approved it, with
// its digest." The issue reads which nodes its request named from the change request at the
// version the person approved, and each node's text from the approved version that approval
// wrote, with that version's provenance - who approved, when, and the request - never from the
// issue's own blocks, which may change after the approval.

let restores: (() => void)[] = [];
afterEach(() => {
  for (const restore of restores) restore();
  restores = [];
});

const APPROVAL = {
  ask: "ask-approval",
  at: "2026-11-22T09:05:00Z",
  digest: "d".repeat(64),
  issue: ISSUE,
  version: 3,
};

/** The spec of `ISSUE` with `approval`. */
function spec(approval: ArtifactApproval | undefined): Artifact {
  return {
    ...(approval === undefined ? {} : { approval }),
    created_at: "2026-11-22T08:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "spec-artifact",
    issue_key: ISSUE,
    kind: "doc",
    name: "spec.md",
    primary: true,
    project: "CORE",
    ref_key: `${ISSUE}/spec`,
    session_id: null,
    slug: "spec-md",
    versions: [],
  };
}

/** The approved nodes the approval of version 3 wrote, by id: the statement it changed, the one it
 *  added, the one it removed, and the check it changed. */
const NODES: Readonly<Record<string, SpecNodeRead>> = {
  "a-bill-names-its-order": approvedNode(
    "a-bill-names-its-order",
    "statement",
    [
      ':::requirement{#a-bill-names-its-order kind="behavior" about="billing"}\nA bill names its order.\n:::\n',
    ],
    APPROVAL,
    { at: APPROVAL.at, version: 1 }
  ),
  "a-refund-returns-the-payment": approvedNode(
    "a-refund-returns-the-payment",
    "statement",
    [
      ':::requirement{#a-refund-returns-the-payment kind="behavior" about="billing"}\nA refund returns the payment of an order.\n:::\n',
    ],
    APPROVAL
  ),
  "a-second-payment-is-refused": approvedNode(
    "a-second-payment-is-refused",
    "check",
    [
      ':::check{#a-second-payment-is-refused serves="an-order-is-paid-once"}\nClaim: A second payment is refused.\n:::\n',
      ':::check{#a-second-payment-is-refused serves="an-order-is-paid-once"}\nClaim: A second payment is refused at once.\n:::\n',
    ],
    APPROVAL
  ),
  "an-order-is-paid-once": approvedNode(
    "an-order-is-paid-once",
    "statement",
    [
      ':::requirement{#an-order-is-paid-once kind="behavior" about="billing"}\nAn order is paid once.\n:::\n',
      ':::requirement{#an-order-is-paid-once kind="behavior" about="billing"}\nAn order is paid once, and a second payment is refused.\n:::\n',
    ],
    APPROVAL
  ),
};

/** After the approval the change request at the approved version reads every node it wrote as
 *  stale (the approved spec now holds them), and its own text is the issue's block at that
 *  version, which this fixture makes differ from the approved text, as an edit of the issue after
 *  the approval does: a view that drew the request's text would show these words. */
function afterApproval() {
  const stale = (id: string, kind: "statement" | "check") =>
    requestNode({
      id,
      kind,
      text: `:::requirement{#${id}}\nThe issue's own words for ${id}.\n:::`,
      what: "stale",
    });
  return changeRequest({
    changed_checks: [stale("a-second-payment-is-refused", "check")],
    proposals: [
      stale("an-order-is-paid-once", "statement"),
      stale("a-refund-returns-the-payment", "statement"),
      stale("a-bill-names-its-order", "statement"),
    ],
  });
}

function renderView(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
}

/** Nodes a row reads in place of `NODES`, by id. */
const NODES_OVERRIDE = new Map<string, SpecNodeRead>();

function mockReads(read: ChangeRequestRead) {
  const getChangeRequest = spyOn(api, "getChangeRequest").mockResolvedValue(read);
  const getSpecNode = spyOn(api, "getSpecNode").mockImplementation(async (_project, _repo, id) => {
    const node = NODES[id];
    if (node === undefined) throw new Error(`no node ${id}`);
    return node;
  });
  restores.push(
    () => getChangeRequest.mockRestore(),
    () => getSpecNode.mockRestore()
  );
  return { getChangeRequest, getSpecNode };
}

test("the issue shows each node its approval wrote, read from the approved version with its provenance, and the digest", async () => {
  const { getChangeRequest, getSpecNode } = mockReads(afterApproval());
  const view = renderView(
    <ApprovedChangeRequest
      issueKey={ISSUE}
      spec={spec({ ask_id: "ask-approval", latest_version: 4, state: "stale", version: 3 })}
    />
  );
  try {
    const section = await screen.findByRole("region", { name: "Approved change request" });
    await waitFor(() => expect(within(section).getAllByRole("listitem")).toHaveLength(4));
    expect(getChangeRequest).toHaveBeenCalledWith(ISSUE, 3);
    expect(getSpecNode).toHaveBeenCalledWith("CORE", "acme/app", "an-order-is-paid-once");
    const rows = within(section).getAllByRole("listitem");
    expect(rows.map((row) => [row.dataset.nodeId, row.dataset.what])).toEqual([
      ["an-order-is-paid-once", "changed"],
      ["a-refund-returns-the-payment", "added"],
      ["a-bill-names-its-order", "removed"],
      ["a-second-payment-is-refused", "changed"],
    ]);
    const texts = rows.map(
      (row) => within(row).getByTestId("approved-change-request-text").textContent
    );
    expect(texts).toEqual([
      "An order is paid once, and a second payment is refused.",
      "A refund returns the payment of an order.",
      "A bill names its order.",
      "Claim: A second payment is refused at once.",
    ]);
    expect(section.textContent).not.toContain("The issue's own words");
    for (const row of rows) {
      const provenance = within(row).getByTestId("approved-change-request-provenance");
      expect(provenance.textContent).toContain("alice");
      expect(within(provenance).getByRole("time").getAttribute("datetime")).toBe(APPROVAL.at);
      expect(
        within(provenance).getByRole("link", { name: "the approval request" }).getAttribute("href")
      ).toContain("ask-approval");
    }
    expect(within(rows[0] as HTMLElement).getByText("version 2")).toBeTruthy();
    expect(within(section).getByTestId("approved-change-request-digest").textContent).toBe(
      APPROVAL.digest
    );
  } finally {
    view.unmount();
  }
});

test("a version of the node another approval wrote is no part of this approval's change request", async () => {
  // The check's last version is the seed's, not this approval's: the request read names the check,
  // but this approval wrote nothing of it, so the issue shows the three statements alone.
  NODES_OVERRIDE.set(
    "a-second-payment-is-refused",
    approvedNode(
      "a-second-payment-is-refused",
      "check",
      [
        ':::check{#a-second-payment-is-refused serves="an-order-is-paid-once"}\nClaim: A second payment is refused.\n:::\n',
      ],
      { ...APPROVAL, ask: "ask-seed", issue: `${"CORE"}-${1}`, version: 1 }
    )
  );
  restores.push(() => NODES_OVERRIDE.clear());
  const getChangeRequest = spyOn(api, "getChangeRequest").mockResolvedValue(afterApproval());
  const getSpecNode = spyOn(api, "getSpecNode").mockImplementation(
    async (_project, _repo, id) => (NODES_OVERRIDE.get(id) ?? NODES[id]) as SpecNodeRead
  );
  restores.push(
    () => getChangeRequest.mockRestore(),
    () => getSpecNode.mockRestore()
  );
  const view = renderView(
    <ApprovedChangeRequest
      issueKey={ISSUE}
      spec={spec({ ask_id: "ask-approval", latest_version: 3, state: "approved", version: 3 })}
    />
  );
  try {
    const section = await screen.findByRole("region", { name: "Approved change request" });
    await waitFor(() => expect(getSpecNode).toHaveBeenCalledTimes(4));
    await waitFor(() =>
      expect(
        within(section)
          .getAllByRole("listitem")
          .map((row) => row.dataset.nodeId)
      ).toEqual(["an-order-is-paid-once", "a-refund-returns-the-payment", "a-bill-names-its-order"])
    );
  } finally {
    view.unmount();
  }
});

test("a read of the approved change request that fails says so, rather than showing nothing", async () => {
  for (const failing of ["change request", "node"] as const) {
    const getChangeRequest = spyOn(api, "getChangeRequest").mockImplementation(async () => {
      if (failing === "change request")
        throw new ApiError(500, { code: "INTERNAL", error: "boom" });
      return afterApproval();
    });
    const getSpecNode = spyOn(api, "getSpecNode").mockImplementation(
      async (_project, _repo, id) => {
        if (id === "a-bill-names-its-order")
          throw new ApiError(500, { code: "INTERNAL", error: "boom" });
        return NODES[id] as SpecNodeRead;
      }
    );
    const view = renderView(
      <ApprovedChangeRequest
        issueKey={ISSUE}
        spec={spec({ ask_id: "ask-approval", latest_version: 3, state: "approved", version: 3 })}
      />
    );
    try {
      expect((await screen.findByRole("alert")).textContent, failing).toBe(
        "Could not read the approved change request."
      );
    } finally {
      view.unmount();
      getChangeRequest.mockRestore();
      getSpecNode.mockRestore();
    }
  }
});

test("an issue of no spec source, an approval not yet given and a request for changes show no approved change request", async () => {
  for (const [name, read, approval] of [
    [
      "no spec source",
      noSourceRequest(),
      { ask_id: "ask-approval", latest_version: 3, state: "approved", version: 3 },
    ],
    ["awaiting", afterApproval(), { latest_version: 3, state: "awaiting" }],
    [
      "changes requested",
      afterApproval(),
      { latest_version: 3, state: "changes_requested", version: 3 },
    ],
    ["no approval", afterApproval(), undefined],
  ] as const) {
    const { getChangeRequest } = mockReads(read);
    const view = renderView(
      <ApprovedChangeRequest
        issueKey={ISSUE}
        spec={spec(approval as ArtifactApproval | undefined)}
      />
    );
    try {
      if (name === "no spec source")
        await waitFor(() => expect(getChangeRequest).toHaveBeenCalled());
      expect(screen.queryByRole("region", { name: "Approved change request" }), name).toBeNull();
      if (name !== "no spec source") expect(getChangeRequest, name).not.toHaveBeenCalled();
    } finally {
      view.unmount();
      for (const restore of restores) restore();
      restores = [];
    }
  }
});

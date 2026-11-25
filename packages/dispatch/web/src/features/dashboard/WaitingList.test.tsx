import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { IssueDetails } from "../../api/types";
import { NoTooltips } from "../issue-page/NoTooltips";
import { CONTROL_LABELS, PRESET_NAMES } from "../issue-page/page-words";
import { AppProviders } from "../issue-page/slots/stage-fixtures";
import { NOTHING_PUBLISHED } from "../refs/reference-names";
import issueBody from "./fixtures/issue.json";
import type { WaitingItem } from "./useWaitingItems";
import { WaitingList } from "./WaitingList";

// A stale approval of what waits (dispatch://INFRA-5702/spec#b-r-in-place, R5): the one approval
// control, inside `NoTooltips`, in the page's words for the latest version.

const ISSUE: IssueDetails = issueBody as unknown as IssueDetails;

test("a stale approval draws the chip with the version approved and the newer one", () => {
  const base = (ISSUE.artifacts ?? []).find(
    (artifact) => artifact.id === ISSUE.verification_artifact_id
  );
  if (base === undefined) throw new Error("the issue fixture holds no Verification document");
  const document = {
    ...base,
    approval: {
      at: "2026-11-25T10:00:00Z",
      by: { id: "alice", kind: "user" },
      latest_version: 3,
      state: "stale",
      version: 2,
    },
  } as typeof base;
  const item: WaitingItem = {
    at: "2026-11-25T10:00:00Z",
    document,
    id: document.id,
    kind: "stale",
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <AppProviders client={client}>
        <NoTooltips>
          <WaitingList
            decisionsDocument={undefined}
            drawn={5}
            issue={ISSUE}
            items={[item]}
            names={{ excerptOf: () => "", published: NOTHING_PUBLISHED }}
            onMore={() => {}}
            viewer="alice"
          />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
  try {
    const drawn = screen.getByTestId(`dashboard-item-stale-${document.id}`);
    expect(drawn.textContent).toContain(PRESET_NAMES.verification);
    expect(
      within(drawn).getByRole("button", {
        name: `Approved v2 · ${CONTROL_LABELS.approval_latest} v3`,
      })
    ).toBeTruthy();
    expect(within(drawn).getByRole("button", { name: "Approve v3" })).toBeTruthy();
    expect(within(drawn).getByRole("button", { name: "Request changes" })).toBeTruthy();
    expect(screen.getByTestId("dashboard-waiting-count").textContent).toBe("1");
  } finally {
    view.unmount();
  }
});

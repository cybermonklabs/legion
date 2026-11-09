import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { ApproverGrantResp } from "../../api/types";
import { GrantsSection } from "./GrantsSection";

/** A grant on a session the viewer operates: the broker allows the operator's revoke, so the row
 *  offers one. */
const operated: ApproverGrantResp = {
  approver: null,
  created_at: "2026-10-03T12:05:00Z",
  decided_by: null,
  enrollment: { kind: "session", operator: "ada@example.com", runtime_id: "box-1", slot: null },
  expires_at: "2026-10-03T13:05:00Z",
  grant_id: "grant-operated",
  granted: "automatic",
  names: ["WORKER_TOKEN"],
  record_id: null,
};

/** A grant that belongs to no person: the policy gave it without asking, so it has no approver,
 *  and a pod acts for nobody, so its enrollment has no operator. The broker refuses every
 *  signed-in person's revoke of it. */
function nobodys(operator: string): ApproverGrantResp {
  return {
    approver: null,
    created_at: "2026-10-03T12:06:00Z",
    decided_by: null,
    enrollment: { kind: "pod", operator, runtime_id: "pod-7", slot: null },
    expires_at: "2026-10-03T13:06:00Z",
    grant_id: "grant-nobodys",
    granted: "automatic",
    names: ["SHARED_TOKEN"],
    record_id: null,
  };
}

/** A grant the broker's model judge approved in the owner's stead: the grant records the owner,
 *  who may end it, and the request records the judge, who decided it. */
const judged: ApproverGrantResp = {
  approver: "ada@example.com",
  created_at: "2026-10-03T12:07:00Z",
  decided_by: "judge:claude-opus-4-6",
  enrollment: { kind: "pod", operator: "", runtime_id: "pod-9", slot: null },
  expires_at: "2026-10-03T12:22:00Z",
  grant_id: "grant-judged",
  granted: "approval",
  names: ["DEEL_API_KEY"],
  record_id: "record-judged",
};

/** A grant ada approved herself, which names her both ways. */
const byAda: ApproverGrantResp = {
  ...judged,
  created_at: "2026-10-03T12:08:00Z",
  decided_by: "ada@example.com",
  grant_id: "grant-by-ada",
  names: ["BILLING_KEY"],
  record_id: "record-by-ada",
};

// The judge decides in the record approver's stead, so a judged grant's approver is the person
// the request waited on and its decider is the judge. The row says who decided it; saying the
// approver named ada for a request she never saw. Her revoke is unchanged: the broker allows it
// because she is the approver, which is what `unrevocable` reads.
test("a judged grant is granted by the judge, a person's own by that person, and both are revocable", async () => {
  const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
    grants: [judged, byAda],
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

  try {
    render(
      <QueryClientProvider client={queryClient}>
        <GrantsSection />
      </QueryClientProvider>
    );
    const section = within(await screen.findByRole("region", { name: "Live grants" }));

    const judgedRow = (await section.findByText("DEEL_API_KEY")).closest("tr") as HTMLElement;
    expect(within(judgedRow).getByText("Approved by judge:claude-opus-4-6")).toBeDefined();
    expect(within(judgedRow).queryByText("Approved by ada@example.com")).toBeNull();
    expect(within(judgedRow).getByRole("button", { name: "Revoke" })).toBeDefined();

    const adasRow = section.getByText("BILLING_KEY").closest("tr") as HTMLElement;
    expect(within(adasRow).getByText("Approved by ada@example.com")).toBeDefined();
  } finally {
    cleanup();
    getCredentialGrants.mockRestore();
  }
});

// A grant with neither an approver nor an operator is on every signed-in person's page so that
// what a pod took is visible, and RevokeByApprover's mayRevoke refuses all of them for it. A
// Revoke button there could only fail, so the row is read-only and says what does end it.
//
// The operator is a plain Go string end to end - `record.Enrollment.Operator`, the
// `wire.RecordEnrollmentResp` the broker writes from it, and the bytes Dispatch relays unparsed
// - so "no operator" arrives as "" and never as null.
for (const operator of [""]) {
  test(`a grant whose operator is ${JSON.stringify(operator)} and approver is null is read-only, and the viewer's own still revokes`, async () => {
    const getCredentialGrants = spyOn(api, "getCredentialGrants").mockResolvedValue({
      grants: [operated, nobodys(operator)],
    });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    try {
      render(
        <QueryClientProvider client={queryClient}>
          <GrantsSection />
        </QueryClientProvider>
      );
      const section = within(await screen.findByRole("region", { name: "Live grants" }));

      const nobodysRow = (await section.findByText("SHARED_TOKEN")).closest("tr") as HTMLElement;
      expect(within(nobodysRow).queryByRole("button", { name: "Revoke" })).toBeNull();
      expect(within(nobodysRow).getByText("Ends with its machine login")).toBeDefined();

      const operatedRow = section.getByText("WORKER_TOKEN").closest("tr") as HTMLElement;
      expect(within(operatedRow).getByRole("button", { name: "Revoke" })).toBeDefined();
      expect(within(operatedRow).queryByText("Ends with its machine login")).toBeNull();

      expect(section.getAllByRole("button", { name: "Revoke" })).toHaveLength(1);
    } finally {
      cleanup();
      getCredentialGrants.mockRestore();
    }
  });
}

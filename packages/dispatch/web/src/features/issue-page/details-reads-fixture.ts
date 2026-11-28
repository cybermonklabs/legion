import { type Mock, spyOn } from "bun:test";

import { api } from "../../api/client";

/** Pinned's reads of the issue's questions, comments and messages (`emptyDetailsReads`), which a
 *  case asserts on or answers with the items its pins name. */
export interface PinReads {
  readonly asks: Mock<typeof api.listIssueAsks>;
  readonly comments: Mock<typeof api.listComments>;
  readonly messages: Mock<typeof api.listIssueMessages>;
}

/** Pinned's reads (`pinReads`), the reader's state read (`getMyState`), which answers the issue
 *  unread and unpinned until a case answers it otherwise, and the one call that puts every read
 *  `emptyDetailsReads` set back. */
export interface DetailsReads {
  readonly getMyState: Mock<typeof api.getMyState>;
  readonly pinReads: PinReads;
  readonly restore: () => void;
}

/**
 * For unit tests only: answers every read open Details makes of the fixture's issue CORE-1, all
 * but the issue's own read, which each caller answers with what its cases hold. The reader's
 * state holds the issue unread and unpinned, and every other read answers the API's empty shape.
 * A read that reached the network would fail the case (`__tests__/network.ts`). The caller puts
 * every read back with `restore`.
 */
export function emptyDetailsReads(): DetailsReads {
  const reads = [
    spyOn(api, "whoAmI").mockResolvedValue({ event_head: "0", kind: "user", login: "alice" }),
    spyOn(api, "listProjects").mockResolvedValue([]),
    spyOn(api, "listAgents").mockResolvedValue([]),
    spyOn(api, "listProjectArchitectureSources").mockResolvedValue([]),
    spyOn(api, "getReferenceTargets").mockResolvedValue({ targets: [] }),
    // Details' People rows count the issue's subscribed agents on every load (`details-more.tsx`).
    spyOn(api, "getIssueSubscribers").mockResolvedValue([]),
    // Details' history reads the issue's log while Details is open (`IssueHistory.tsx`), its step
    // records, each run an entry, and the list of its handoffs.
    spyOn(api, "getIssueEvents").mockResolvedValue([]),
    spyOn(api, "listSteps").mockResolvedValue({ issue: "CORE-1", tasks: [] }),
    spyOn(api, "listHandoffs").mockResolvedValue([]),
  ];
  const pinReads: PinReads = {
    asks: spyOn(api, "listIssueAsks").mockResolvedValue([]),
    comments: spyOn(api, "listComments").mockResolvedValue([]),
    messages: spyOn(api, "listIssueMessages").mockResolvedValue([]),
  };
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({
    "CORE-1": { dismissed: [], last_read_seq: 0, pinned: false, seq: 0 },
  });
  return {
    getMyState,
    pinReads,
    restore: () => {
      for (const spy of reads) spy.mockRestore();
      getMyState.mockRestore();
      for (const spy of Object.values(pinReads)) spy.mockRestore();
    },
  };
}

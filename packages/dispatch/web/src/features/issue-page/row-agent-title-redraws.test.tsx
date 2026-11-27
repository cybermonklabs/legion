import { expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent } from "../../api/types";
import { PHONE_WIDTH, stubViewport } from "../margin/margin-fixture";
import { useAgentText } from "../refs/AgentName";
import { agentsQuery } from "../refs/agent-registry";
import { parseIssuePath } from "../refs/routes";
import { issueDetails, KEY } from "./decisions-filter/decisions-fixtures";
import { presetPath } from "./issue-address";
import { IssueTreeOwnerProvider } from "./issue-tree-owner";
import { TWO_THREADS } from "./outline-fixtures";
import { recordRowDraws, treeOfComments } from "./row-draw-fixtures";
import { ProvidersSlot } from "./slots/providers";
import { StageSlot } from "./slots/stage";
import { AppProviders, stubReads, treeOf } from "./slots/stage-fixtures";

const AGENT: Agent = {
  capabilities: [],
  dir: "/workspace",
  host: "worker",
  last_activity: null,
  last_seen: 1_000,
  machine_id: "worker",
  open_asks: 0,
  roles: [],
  session_id: "agent-session",
  state: "live",
  title: "Reviewer",
};

test("a registry heartbeat does not redraw new issue row text when the names stay the same", async () => {
  const restoreViewport = stubViewport(PHONE_WIDTH);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const path = presetPath(KEY, "conversation");
  const route = parseIssuePath(path);
  if (route === undefined) throw new Error("the fixture names no issue");
  const issue = issueDetails();
  const tree = treeOfComments(TWO_THREADS);
  const stubs = stubReads();
  const treeRead = spyOn(api, "getIssueTree").mockImplementation(async (_key, query = {}) =>
    query.view === undefined ? tree : treeOf([])
  );
  const { queryKey } = agentsQuery();
  client.setQueryData(queryKey, [AGENT]);
  const draws = recordRowDraws();
  let outside = 0;
  function RegistryReader({ legacy = false }: { readonly legacy?: boolean }): ReactNode {
    const names = useAgentText();
    if (legacy) outside += 1;
    return <span>{names.sessionText(AGENT.session_id, undefined)}</span>;
  }
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <AppProviders client={client}>
        <RegistryReader legacy />
        <ProvidersSlot issue={issue} route={route}>
          <IssueTreeOwnerProvider issue={issue}>
            <RegistryReader />
            <StageSlot issue={issue} route={route} />
          </IssueTreeOwnerProvider>
        </ProvidersSlot>
      </AppProviders>
    </MemoryRouter>
  );
  try {
    await waitFor(() => {
      expect(Object.keys(draws.since(0)).length).toBeGreaterThan(0);
      expect(client.isFetching()).toBe(0);
    });
    const mark = draws.mark();
    const legacyBefore = outside;
    await act(async () => {
      client.setQueryData(queryKey, [{ ...AGENT, last_seen: 2_000 }]);
    });
    await waitFor(() => expect(outside).toBeGreaterThan(legacyBefore));
    expect(draws.since(mark), "a heartbeat redrew the outline's words").toEqual({});

    await act(async () => {
      client.setQueryData(queryKey, [{ ...AGENT, last_seen: 3_000, title: "Tester" }]);
    });
    await waitFor(() => expect(view.getAllByText("Tester")).toHaveLength(2));
  } finally {
    view.unmount();
    draws.restore();
    treeRead.mockRestore();
    for (const stub of stubs) stub.mockRestore();
    restoreViewport();
  }
});

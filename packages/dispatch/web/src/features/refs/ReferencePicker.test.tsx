import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: user-event cannot mark a key as an input method's (`isComposing`, keyCode 229), which the filter row below reads
import { fireEvent, render, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { IssueSummary } from "../../api/types";
import { issue } from "../margin/margin-fixture";
import { ReferencePicker } from "./ReferencePicker";

/** The project's one other issue, as the list of its issues answers it. */
const related: IssueSummary = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: issue.components,
  created_at: issue.created_at,
  created_by: issue.created_by,
  key: "CORE-2",
  labels: [],
  last_seq: 1,
  open_asks: 0,
  parent: null,
  primary_artifact_id: "",
  priority: null,
  progress: { tasks: null, children: null },
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
  status: "todo",
  title: "Related work",
  updated_at: issue.updated_at,
};

// A reader typing Chinese, Japanese or Korean confirms a word with Enter: that Enter is the input
// method's, marked `isComposing`, or sent unmarked with keyCode 229 after the composition's end, as
// Safari sends it (WebKit bug 165004). In the picker's filter it picks no reference; a plain Enter
// after it picks the top match.
test("the Enter that confirms an input method's word in the filter picks no reference, and a plain Enter picks the top match", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue(issue);
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([related]);
  const picked: string[] = [];
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <ReferencePicker issueKey="CORE-1" onClose={() => {}} onSelect={(ref) => picked.push(ref)} />
    </QueryClientProvider>
  );
  try {
    const scope = within(view.container);
    await scope.findByRole("button", { name: "CORE-2: Related work" });
    const filter = scope.getByRole("textbox", { name: "Filter issues" });
    // user-event cannot mark a key as an input method's, so the two such Enters are fired.
    expect(fireEvent.keyDown(filter, { isComposing: true, key: "Enter" })).toBe(true);
    expect(fireEvent.keyDown(filter, { isComposing: false, key: "Enter", keyCode: 229 })).toBe(
      true
    );
    expect(picked).toEqual([]);

    // The plain Enter goes the same way as the two above, so only the composition mark differs.
    expect(fireEvent.keyDown(filter, { key: "Enter" })).toBe(false);
    expect(picked).toEqual(["dispatch://CORE-2"]);
  } finally {
    view.unmount();
    client.clear();
    getIssue.mockRestore();
    listIssues.mockRestore();
  }
});

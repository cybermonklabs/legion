import { act, type RenderHookResult, renderHook } from "@testing-library/react";

import type { Issue } from "../../api/types";
import {
  type IssueDrafts,
  type IssueUpdateInput,
  type IssueUpdateMutation,
  useIssueDrafts,
} from "./useIssueDrafts";

// The issue and the mounted hook that both of useIssueDrafts' test files play against: the orders
// of useIssueDrafts.test.ts, each a test of its own, and the sweep of every order up to five events
// long in useIssueDrafts.exploration.test.ts.

export const startIssue: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
  verification_artifact_id: "",
};

interface HookProps {
  issue: Issue;
  updateIssue: IssueUpdateMutation;
}

/**
 * One mounted hook, the fake mutation it writes through, and the issue its prop holds. The fake
 * records each write in order and never answers by itself: when a write lands or is refused is the
 * caller's choice, since the orders of those answers are what both test files are about.
 */
export class Session {
  readonly received: IssueUpdateInput[] = [];
  readonly updateIssue: IssueUpdateMutation = {
    mutate: (input) => {
      this.received.push(input);
      this.updateIssue.variables = input;
    },
    // The keyed write's Retry makes its newest press again; the fake receives it as a write.
    retry: () => {
      const { variables } = this.updateIssue;
      if (variables !== undefined) this.updateIssue.mutate(variables);
    },
    variables: undefined,
  };
  issue = startIssue;
  /** The write the fake last received that has not landed or been refused. */
  out: IssueUpdateInput | null = null;
  private readonly hook: RenderHookResult<IssueDrafts, HookProps>;

  constructor() {
    // The hook draws nothing, so it mounts in a container of its own outside the document. Each
    // container put into the page's body and taken out again made happy-dom look the body up
    // afresh and keep a record of that lookup, two a mount, which the sweep's mounts add up.
    this.hook = renderHook((props: HookProps) => useIssueDrafts(props.issue, props.updateIssue), {
      container: document.createElement("div"),
      initialProps: { issue: this.issue, updateIssue: this.updateIssue },
    });
  }

  get drafts(): IssueDrafts {
    return this.hook.result.current;
  }

  /** Runs one step inside `act` and returns the writes the fake received during it. */
  step(action: () => void): IssueUpdateInput[] {
    const before = this.received.length;
    act(action);
    const sent = this.received.slice(before);
    const last = sent.at(-1);
    if (last !== undefined) this.out = last;
    return sent;
  }

  type(value: string): IssueUpdateInput[] {
    return this.step(() => this.drafts.writeTitle(value));
  }

  /** The end of an edit: a blur or Enter. The new header's switch or page hide, which sends the
   *  draft and leaves the edit open, is `hide`. */
  end(): IssueUpdateInput[] {
    return this.step(() => this.drafts.requestTitleSubmit());
  }

  escape(): IssueUpdateInput[] {
    return this.step(() => this.drafts.discardTitle());
  }

  /**
   * Whether a hide asks the hook for the draft: a switch to another window, or a hide of the
   * page, with the new header's editor open sends the editor's draft and leaves the edit open
   * (`sendAway` in `IssueTitleRow.tsx`). It asks for a draft that is not empty and is changed,
   * as `sendAway` does; a title queued behind a write while the draft is typed back asks for
   * nothing, and stays the reader's last ended choice (decision 324). The title row's own guard
   * against sending a draft twice (`went`) is the row's, which the header's cases hold.
   */
  hideAsks(): boolean {
    const { drafts } = this;
    return drafts.title.trim() !== "" && drafts.titleDirty;
  }

  hide(): IssueUpdateInput[] {
    return this.step(() => {
      if (this.hideAsks()) this.drafts.requestTitleSubmit();
    });
  }

  /** The write out lands: the issue takes the title it carried, then Dispatch's answer arrives. */
  land(): IssueUpdateInput[] {
    const input = this.takeOut();
    const shown =
      input.title === undefined ? [] : this.showIssue({ ...this.issue, title: input.title });
    const next = this.issue;
    const answered = this.step(() => {
      this.drafts.onIssueSuccess(next, input);
      this.drafts.onIssueSettled(null);
    });
    return [...shown, ...answered];
  }

  refuse(): IssueUpdateInput[] {
    this.takeOut();
    return this.step(() => this.drafts.onIssueSettled(new Error("refused")));
  }

  otherReader(title: string): IssueUpdateInput[] {
    return this.showIssue({ ...this.issue, title });
  }

  /**
   * Another field's write lands carrying another reader's title, as the new header's status pick
   * does. Its answer reaches the hook, but it is not the hook's own write, so nothing settles.
   */
  otherFieldLands(title: string): IssueUpdateInput[] {
    const next = { ...this.issue, title, status: "in_progress" };
    const answered = this.step(() => this.drafts.onIssueSuccess(next, { status: "in_progress" }));
    return [...answered, ...this.showIssue(next)];
  }

  titlesSent(): string[] {
    return this.received.flatMap((input) => (input.title === undefined ? [] : [input.title]));
  }

  private takeOut(): IssueUpdateInput {
    const input = this.out;
    if (input === null) throw new Error("no write is out");
    this.out = null;
    return input;
  }

  private showIssue(next: Issue): IssueUpdateInput[] {
    this.issue = next;
    return this.step(() => this.hook.rerender({ issue: next, updateIssue: this.updateIssue }));
  }
}

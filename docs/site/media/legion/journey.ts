// Example issues going through Legion, as Dispatch records them. Each move below is one the Legion
// daemon, its agents or the human makes, written through the API they write through, so the Legion
// section's screenshots (`shots.config.ts` beside this file) and its narrated walkthrough
// (`../walkthroughs/legion-issue-journey.ts`) show what a real journey leaves in Dispatch. Every
// name, key, commit and URL is example data.
import { dispatchApi, expect, fakeEnvoy } from "../harness";

export const PROJECT = "SHOP";

/** The Legion daemon's own writes: the status it sets on the issue. */
export const DAEMON = {
  actor: { id: "legion-daemon:SHOP", kind: "session" },
  as: "agent",
} as const;

/** The project's controller session, which posts the daily report. */
export const CONTROLLER = {
  actor: { id: "shop-controller", kind: "session", origin: { session_title: "SHOP controller" } },
  as: "agent",
} as const;

export type AgentRole = "architect" | "implementer";

/** One of an issue's Legion sessions, named as Legion names its panes: `<KEY> <role>`. */
export function agent(key: string, role: AgentRole) {
  const session = `${key.toLowerCase()}-${role}`;
  return {
    actor: { id: session, kind: "session", origin: { session_title: `${key} ${role}` } },
    as: "agent",
  } as const;
}

/** What one example issue says at each step of its journey. */
export interface Example {
  readonly title: string;
  readonly spec: string;
  /** The design the architect adds to the spec, with one decision block. */
  readonly design: string;
  /** The issue's Verification document, which the architect writes with its design: one
   *  `before_merge` check for each requirement of the spec, which `key`, the issue's key, names. */
  readonly checks: (key: string) => string;
  /** The decision block's id and the option the human picks. */
  readonly decision: { readonly id: string; readonly pick: string };
  readonly approvalSummary: string;
  readonly pullRequest: { readonly number: number; readonly url: string };
  /** The head the merger merges. */
  readonly head: string;
  readonly merge: string;
  /** The architect's acceptance of the reviewed head, posted before retro and the merge. */
  readonly acceptance: string;
}

export const SAVED_CART: Example = {
  title: "Let shoppers save their cart for later",
  spec: `# Save a cart for later

A signed-in shopper can save the cart they are building and pick it up again later, on any device.

## Requirements

:::requirement{#r1}
A signed-in shopper saves the cart they are building, and the saved cart holds every line of it and its quantity.
:::

:::requirement{#r2}
A saved cart belongs to the shopper's account rather than to the device that saved it, so any device they sign in on opens it.
:::

:::requirement{#r3}
A saved cart nobody has touched for 30 days is removed, and the shopper is told a week before it goes.
:::
`,
  design: `## Design

Saving copies the cart's lines into a saved cart the account owns. Opening a saved cart replaces the
current cart, after asking first when the current cart has items.

:::ask{#cart-storage urgency="high" multiple="false"}
Where should a saved cart live?

I recommend Server: the second requirement needs the cart on any device the shopper signs in on, which browser storage cannot give.

- Server: one saved cart per account in the database, so any device can open it
- Browser: local storage on the device that saved it, with no schema change
:::
`,
  checks: (key) => `# Verification

:::check{#c-save serves="dispatch://${key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/saved-cart.e2e.ts" when="before_merge" negative_control="Save the cart without its quantities, and the test fails on the first line it reads back."}
A shopper saves a two-line cart from the cart page, and the saved cart holds both lines at their quantities.
:::

:::check{#c-device serves="dispatch://${key}/spec#b-r2" surface="browser" run="bun run e2e -- e2e/saved-cart.e2e.ts" when="before_merge" negative_control="Keep the saved cart in the browser's own storage, and the second browser opens no cart."}
A cart saved in one browser opens in a second browser signed in to the same account, with the same lines and quantities.
:::

:::check{#c-expiry serves="dispatch://${key}/spec#b-r3" surface="cli" run="go test ./internal/carts -run TestAnUntouchedCartExpires" when="before_merge" negative_control="Send no warning email, and the test fails at day 23."}
A cart untouched for 23 days gets its shopper a warning email, and one untouched for 30 days is removed.
:::
`,
  decision: { id: "cart-storage", pick: "Server" },
  approvalSummary:
    "Saved carts live on the server, one per account, and open on any device; a nightly job removes carts untouched for 30 days, a week after emailing the shopper.",
  pullRequest: { number: 42, url: "https://github.com/acme/storefront/pull/42" },
  head: "9c41e07",
  merge: "4b7d2a1",
  acceptance:
    "Signed off. Each requirement's `before_merge` check reads yes at 9c41e07 in `dispatch results`, and the reviewer approved that head with no open threads. Retro and the merge follow; the tester's production check follows the merge, on the pull request.",
};

export const DELIVERY_DATE: Example = {
  title: "Show the delivery date at checkout",
  spec: `# Show the delivery date at checkout

A shopper sees when their order will arrive before they pay.

## Requirements

:::requirement{#r1}
Checkout shows a delivery date beside every shipping option, before the shopper pays.
:::

:::requirement{#r2}
Each date is the one that shipping option would deliver on, so a shopper who picks another option sees that option's date.
:::
`,
  design: `## Design

The checkout page asks for a date for every shipping option when it loads, and shows each beside its
option.

:::ask{#date-source urgency="high" multiple="false"}
Where should the delivery dates come from?

I recommend Carrier: its estimate already counts the warehouse's cut-off time and the carrier's holidays.

- Carrier: the carrier's estimate for each shipping option
- Table: business days per shipping option, kept in the store's settings
:::
`,
  checks: (key) => `# Verification

:::check{#c-dates serves="dispatch://${key}/spec#b-r1" surface="browser" run="bun run e2e -- e2e/checkout-dates.e2e.ts" when="before_merge" negative_control="Leave one shipping option without its date, and the test fails naming it."}
Checkout shows a delivery date beside each shipping option, above the button that pays.
:::

:::check{#c-option serves="dispatch://${key}/spec#b-r2" surface="browser" run="bun run e2e -- e2e/checkout-dates.e2e.ts" when="before_merge" negative_control="Show the first option's date beside every option, and the test fails on the second."}
Choosing another shipping option shows the carrier's date for that option.
:::
`,
  decision: { id: "date-source", pick: "Carrier" },
  approvalSummary:
    "Checkout asks the carrier for each shipping option's delivery date when it loads, and shows the date beside the option.",
  pullRequest: { number: 43, url: "https://github.com/acme/storefront/pull/43" },
  head: "2f8e6c1",
  merge: "7d03b9e",
  acceptance:
    "Signed off. Each requirement's `before_merge` check reads yes at 2f8e6c1 in `dispatch results`, and the reviewer approved that head with no open threads. Retro and the merge follow; the tester's production check follows the merge, on the pull request.",
};

/** An example issue in Dispatch: its key and its spec document. */
export interface Tracked {
  readonly key: string;
  readonly spec: string;
}

/** Creates the project and marks every Legion session the examples name live in the fake Envoy,
 *  so Dispatch shows each as a running agent. */
export async function seedProject(keys: readonly string[]): Promise<void> {
  const api = await dispatchApi();
  const { setLiveSessions } = await fakeEnvoy();
  await setLiveSessions(
    keys.flatMap((key) =>
      (["architect", "implementer"] as const).map((role) => ({
        capabilities: ["aside", "btw", "steer"],
        dir: `/workspaces/${key.toLowerCase()}-${role}`,
        machine_id: "example-host-legion",
        roles: [`legion-shop-${key.toLowerCase()}-${role}`],
        session_id: `${key.toLowerCase()}-${role}`,
        title: `${key} ${role}`,
      }))
    )
  );
  await api.createProject({ key: PROJECT, name: "Storefront" });
}

/** A human files the issue with its spec, which states its requirements. The Spec tab lists them
 *  under the document (`web/src/features/issue/SpecRequirements.tsx`), read out of the block
 *  index that the accepted write fills, so this waits until Dispatch lists each one: a page opened
 *  before then shows the panel's empty state, which a shot or a walkthrough section refuses. Each
 *  requirement is flagged `has no check` until the architect writes the issue's Verification
 *  document (`writeDesign`). */
export async function file(example: Example): Promise<Tracked> {
  const api = await dispatchApi();
  const issue = await api.createIssue({
    project: PROJECT,
    spec: example.spec,
    title: example.title,
  });
  const requirements = example.spec.match(/^:::requirement\{/gm)?.length ?? 0;
  await expect
    .poll(async () => (await api.getIssueCoverage(issue.key)).requirements?.length ?? 0)
    .toBe(requirements);
  return { key: issue.key, spec: issue.primary_artifact_id };
}

/** The handover: a human labels the issue `legion` and sets it to todo. */
export async function handOver(issue: Tracked): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(issue.key, { labels: ["legion"], priority: 1, status: "todo" });
}

/** The daemon admits the issue, and its architect claims it. */
export async function admit(issue: Tracked): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(issue.key, { status: "in_progress" }, DAEMON);
  await api.claimIssue(issue.key, agent(issue.key, "architect"));
}

/** The architect writes its design, with one decision block, into the spec, and the issue's
 *  Verification document, one `before_merge` check for each requirement. The block becomes an ask
 *  once the document service settles the edit, and each requirement's `has no check` flag clears
 *  once Dispatch indexes the check that serves it. */
export async function writeDesign(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  await api.editArtifact(
    issue.spec,
    { ops: [{ after: "end", markdown: example.design, op: "insert" }] },
    agent(issue.key, "architect")
  );
  await api.createIssueArtifact(
    issue.key,
    { content: example.checks(issue.key), name: "verification.md" },
    agent(issue.key, "architect")
  );
  await expect.poll(async () => (await api.getIssue(issue.key)).open_asks?.length ?? 0).toBe(1);
  await expect
    .poll(async () => {
      const requirements = (await api.getIssueCoverage(issue.key)).requirements ?? [];
      return requirements.length > 0 && requirements.every((row) => (row.checks ?? 0) > 0);
    })
    .toBe(true);
}

/** The number of versions the spec has, to wait for the one an answer writes. */
export async function specVersions(issue: Tracked): Promise<number> {
  const api = await dispatchApi();
  return (await api.getArtifact(issue.spec)).versions?.length ?? 0;
}

/** Waits until the spec has more versions than `before`: an answer is written into its decision
 *  block as a new version, and an approval asked for before that version lands would move to it
 *  and wait on the architect to hand it back. */
export async function specVersionAfter(issue: Tracked, before: number): Promise<void> {
  const api = await dispatchApi();
  await expect
    .poll(async () => (await api.getArtifact(issue.spec)).versions?.length ?? 0)
    .toBeGreaterThan(before);
}

/** The human answers the decision block. */
export async function answerDecision(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  const [decision] = (await api.getIssue(issue.key)).open_asks ?? [];
  if (decision === undefined) {
    throw new Error(`journey: ${issue.key} has no open ask to answer`);
  }
  const versions = await specVersions(issue);
  await api.answerAsk(decision.id, { expected_edited_at: null, selected: [example.decision.pick] });
  await specVersionAfter(issue, versions);
}

/** The architect asks the human to approve the spec, with its summary; answers the ask's id. */
export async function requestSpecApproval(issue: Tracked, example: Example): Promise<string> {
  const api = await dispatchApi();
  const requested = await api.requestApproval(
    issue.spec,
    { summary: example.approvalSummary },
    agent(issue.key, "architect")
  );
  if (requested.ask === null) {
    throw new Error(`journey: the approval request on ${issue.key}'s spec opened no ask`);
  }
  return requested.ask.id;
}

/** The human approves the spec. */
export async function approve(approval: string): Promise<void> {
  const api = await dispatchApi();
  await api.answerAsk(approval, { expected_edited_at: null, selected: ["Approve"] });
}

/** The implementer links the pull request it opened. */
export async function linkPullRequest(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(
    issue.key,
    { external_links: [{ url: example.pullRequest.url }] },
    agent(issue.key, "implementer")
  );
}

/** The statuses the daemon sets as the phases after implementation run, up to the reviewer's
 *  approval; the architect's acceptance (`accept`) sets the next. */
export const PHASE_STATUSES = ["testing", "needs_review"] as const;

/** The daemon sets one status. */
export async function setStatus(issue: Tracked, status: string): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(issue.key, { status }, DAEMON);
}

/** The reviewer approved, so on the default steps the issue is in accepting: the architect reads
 *  the state of each `before_merge` check at the reviewed head in `dispatch results`, posts its
 *  acceptance of that head and signs off, and, the acceptance being the task's last step, the
 *  daemon moves the issue to retro. The example records no check result, so it shows the
 *  acceptance alone. */
export async function accept(issue: Tracked, example: Example): Promise<void> {
  const api = await dispatchApi();
  await api.createComment(issue.key, { body: example.acceptance }, agent(issue.key, "architect"));
  await api.patchIssue(issue.key, { status: "retro" }, DAEMON);
}

/** The merge closes the issue: the daemon writes done. The production check the project's
 *  `production_check` opens — the tester's, unless the setting is `off` — follows on the pull
 *  request, which the example's GitHub does not show, and the example records no result of it on
 *  the issue. Each look that finds the deploy missing names its own next look with `wake_at`, and
 *  nothing else wakes the check. */
export async function closeAtMerge(issue: Tracked): Promise<void> {
  const api = await dispatchApi();
  await api.patchIssue(issue.key, { status: "done" }, DAEMON);
}

/** The whole journey, handover to the merge that closes the issue, with no stop between the moves. */
export async function finish(issue: Tracked, example: Example): Promise<void> {
  await handOver(issue);
  await admit(issue);
  await writeDesign(issue, example);
  await answerDecision(issue, example);
  await approve(await requestSpecApproval(issue, example));
  await linkPullRequest(issue, example);
  for (const status of PHASE_STATUSES) await setStatus(issue, status);
  await accept(issue, example);
  await closeAtMerge(issue);
}

const REPORT_SPEC =
  "## Summary\n\nLegion's controller posts one message here each day: what Legion finished, what it closed without a change and why, and what is running. This issue is not work, so it carries no `legion` label and stays in icebox.\n";

/** The controller creates the project's `Legion daily report` issue, parks it in icebox through
 *  the daemon, and posts the day's report, naming each issue that closed today with its pull
 *  request. Answers the report issue. */
export async function postDailyReport(
  finished: readonly { readonly issue: Tracked; readonly example: Example }[]
): Promise<Tracked> {
  const api = await dispatchApi();
  const report = await api.createIssue(
    { project: PROJECT, spec: REPORT_SPEC, title: "Legion daily report" },
    CONTROLLER
  );
  await api.patchIssue(report.key, { status: "icebox" }, DAEMON);
  const lines = finished.map(
    ({ issue, example }) => `- ${issue.key} ${example.title}: merged in ${example.pullRequest.url}`
  );
  await api.createMessage(
    report.key,
    {
      body: [
        `Finished:\n${lines.join("\n")}`,
        "Closed without a change: none.",
        "Running: no tree is live, no issue holds a slot, and nothing is waiting.",
        "Free slots: 4 of 4 issues. Each stays free until a person hands Legion an issue.",
      ].join("\n\n"),
    },
    CONTROLLER
  );
  return { key: report.key, spec: report.primary_artifact_id };
}

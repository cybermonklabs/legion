/** The row reads the server's stop of a Do run whose conversation the bar keeps: an Esc in the
 *  bar's empty box is the server's stop of a run whose model call the fake records its caller
 *  leaving, and the run's state the bar draws is the one the server keeps. */
import { createIssue, createProject } from "./api";
import {
  callTool,
  decided,
  endlessStream,
  modelsLane,
  prose,
  runCalls,
  scriptChat,
  scriptDecision,
} from "./fake-models-helpers";
import {
  bar,
  lineOnTop,
  newestRun,
  openBarOver,
  RUN_STEPS_MS,
  searchBox,
  sidePanel,
} from "./going-locators";
import { expect, test } from "./test";
import { withAlice } from "./users";

// Spec R10 of dispatch://INFRA-1588/spec: "Stop, or Esc in the empty box of the bar, stops a run
// at once". An Ask run is handed to the panel at the side, where going-agent-background.e2e.ts
// holds Esc in the panel's box. A Do run keeps its conversation in the bar, and its Esc is the
// bar's own (SearchPalette.tsx). No row held that one: with the bar's Esc made to stop nothing,
// every row of the models lane still passed (the tester's farm at 31b66fa1, mutant esc-bar).
// This row holds it. Negative control: that mutant fails this row, the run still working.
// Every row runs in the models lane alone (`bun run e2e:models`).

// SLOW(>=2s): Jev's Do for the words, a run that never ends, and Esc in the bar.
test("Esc in the bar's empty box while a Do run works stops it, and the bar stays open", async ({
  browser,
}) => {
  modelsLane();
  test.setTimeout(120_000);
  await createProject({ key: "CORE", name: "Core" });
  const key = (await createIssue({ project: "CORE", title: "Track the comet" })).key;
  await scriptDecision({ default: decided("dispatch_issue_update", 0.99) });
  const words = "mark the comet work as started once you have read it all";
  await scriptChat({
    default: prose("On it."),
    runs: [
      {
        words,
        afterwards: endlessStream(),
        turns: [callTool("dispatch_read", { issue: key }), endlessStream()],
      },
    ],
  });
  await withAlice(browser, async (page) => {
    await openBarOver(page, "/projects/CORE/issues");
    await searchBox(page).fill(words);
    await expect(lineOnTop(page)).toContainText("Do", { timeout: RUN_STEPS_MS });
    await searchBox(page).press("Enter");
    await expect(bar(page)).toHaveAttribute("data-state", "conversation", {
      timeout: RUN_STEPS_MS,
    });
    await expect(sidePanel(page)).toHaveCount(0);
    await expect(newestRun(page).getByTestId("going-step").first()).toHaveAttribute(
      "data-running",
      "false",
      { timeout: RUN_STEPS_MS }
    );
    await expect(newestRun(page)).toHaveAttribute("data-state", "working");
    await expect(searchBox(page)).toHaveValue("");
    await searchBox(page).press("Escape");
    await expect(newestRun(page)).toHaveAttribute("data-state", "stopped_person", {
      timeout: RUN_STEPS_MS,
    });
    // The bar stays open with its conversation, and the run's model call has ended.
    await expect(bar(page)).toBeVisible();
    await expect
      .poll(async () => (await runCalls(words)).at(-1)?.left ?? null, { timeout: 5_000 })
      .not.toBeNull();
  });
});

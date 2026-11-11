import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, test } from "@playwright/test";

import { createIssue, createProject, createProjectDocument, getAsk } from "./api";
import { runDispatch } from "./dispatch-cli";
import { gotoIssue } from "./issue-page";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * dispatch://INFRA-1576 on the surface the asking agent reads: `dispatch open-asks` names the
 * person each of its open questions waits on, by the server's own expression
 * (`coalesce(a.asked_of, i.assignee)`, answered as `waiting_on_person`), so an agent that opened
 * a question can say who it is waiting for instead of guessing.
 *
 * The real command, as its own process, opens both questions through the real server: one on an
 * issue a person holds, one on a project document, which nobody holds. A second person then
 * hands the first to themselves in a browser, and the same author session's `open-asks` names
 * them from then on.
 */

/** The `- ` lines of one `dispatch open-asks` run, each one open ask. */
function printedRows(stdout: string): string[] {
  return stdout.split("\n").filter((line) => line.startsWith("- "));
}

/** The one printed row for an ask, by the deep link each row ends in, or a failure naming what
 *  was printed instead: an ask the listing left out is the command's answer being wrong. */
function rowFor(stdout: string, askId: string): string {
  const rows = printedRows(stdout);
  const row = rows.find((line) => (line.split(" · ").at(-1) ?? "").includes(askId));
  expect(row, `no open-asks row names ${askId}: ${rows.join(" | ")}`).toBeDefined();
  return row ?? "";
}

/** The three numbers the listing carries: its total, and the size of each of its two groups. */
function printedCounts(stdout: string): { agent: number; human: number; total: number } {
  const number = (pattern: RegExp): number => {
    const found = pattern.exec(stdout);
    expect(found, `${pattern.source} is not in: ${stdout}`).not.toBeNull();
    return Number(found?.[1] ?? -1);
  };
  return {
    agent: number(/^Waiting on agent \((\d+)\):$/m),
    human: number(/^Waiting on human \((\d+)\):$/m),
    total: number(/^(\d+) unanswered asks? you authored/m),
  };
}

/** The id of the ask a `dispatch ask` run opened, from the line it prints. */
function askedId(stdout: string): string {
  const id = /^Asked ([0-9a-f-]{36}) on /m.exec(stdout)?.[1];
  expect(id, `dispatch ask printed no ask id: ${stdout}`).toBeDefined();
  return id ?? "";
}

test.beforeEach(async () => {
  await resetDatabase();
});

// SLOW(>=2s): five cold `bun` starts of the real `dispatch` command, each its own process,
// around one hand-on made in a browser.
test("open-asks names the person each ask waits on, before and after a hand-on", async ({
  browser,
}, testInfo) => {
  test.skip(testInfo.project.name !== "chromium", "the command runs outside the browser");
  test.setTimeout(240_000);
  await createProject({ key: "CORE", name: "Core" });
  // A human's issue is assigned to its creator, so this one is alice's; a project document has
  // no assignee, so nobody waits on a question asked on it.
  const issue = await createIssue(
    { project: "CORE", title: "Ship the rollout flag" },
    { login: "alice" }
  );
  expect(issue.assignee).toBe("alice");
  const runbook = await createProjectDocument("CORE", {
    content: "# Runbook\n\nRestart, reseed, reopen.\n",
    name: "Runbook",
  });

  const home = mkdtempSync(join(tmpdir(), "dispatch-asked-of-open-asks-"));
  const bob = await asUser(browser, "bob");
  try {
    const opened = await runDispatch(
      [
        "ask",
        "--issue",
        issue.key,
        "--question",
        "The rollout reads one flag name and the runbook names another. Which one ships?",
      ],
      home
    );
    expect(opened.code, `${opened.stdout}${opened.stderr}`).toBe(0);
    const issueAsk = askedId(opened.stdout);

    const onDocument = await runDispatch(
      [
        "ask",
        "--project",
        "CORE",
        "--artifact",
        runbook.artifact.slug,
        "--question",
        "The runbook is the only copy of the restart steps. Keep it in the repository?",
      ],
      home
    );
    expect(onDocument.code, `${onDocument.stdout}${onDocument.stderr}`).toBe(0);
    const documentAsk = askedId(onDocument.stdout);

    // 1. The session's own listing names the issue's assignee on the first row and nobody on the
    // second - in the card's own word, `asked of`, which claims no turn - and both are waiting
    // on a human, so the three numbers are 2, 2 and 0.
    const own = await runDispatch(["open-asks"], home);
    expect(own.code, `${own.stdout}${own.stderr}`).toBe(0);
    expect(rowFor(own.stdout, issueAsk)).toContain(" · asked of alice · ");
    expect(rowFor(own.stdout, documentAsk)).toContain(" · asked of nobody · ");
    expect(printedCounts(own.stdout)).toEqual({ agent: 0, human: 2, total: 2 });

    // 2. The project-wide listing, which is every open ask in the project whoever authored it,
    // names the same two people and carries the same three numbers.
    const project = await runDispatch(["open-asks", "--project", "CORE"], home);
    expect(project.code, `${project.stdout}${project.stderr}`).toBe(0);
    expect(rowFor(project.stdout, issueAsk)).toContain(" · asked of alice · ");
    expect(rowFor(project.stdout, documentAsk)).toContain(" · asked of nobody · ");
    expect(printedCounts(project.stdout)).toEqual({ agent: 0, human: 2, total: 2 });

    // 3. Bob, who holds neither the issue nor the document, takes the first question himself
    // with the control on its card on the issue page. The address is the one the Conversation
    // tab answered and still resolves (dispatch://INFRA-1196 R19), since the redesigned page
    // draws no such tab.
    const page = await bob.newPage();
    await gotoIssue(page, `/issues/${issue.key}/conversation`);
    const card = page.locator("#issue-conversation-panel").getByTestId(`ask-${issueAsk}`);
    await expect(card).toContainText("Asked of alice");
    const control = card.locator(`select[data-ask-hand-on="${issueAsk}"]`);
    await control.focus();
    await expect(control.locator('option[value="bob"]')).toHaveCount(1);
    await control.selectOption("bob");
    await expect(card).toContainText("Asked of bob");
    await expect
      .poll(async () => (await getAsk(issueAsk, { login: "bob" })).ask.asked_of)
      .toBe("bob");

    // 4. The same author session's listing names him from then on, with nothing else moved.
    const after = await runDispatch(["open-asks"], home);
    expect(after.code, `${after.stdout}${after.stderr}`).toBe(0);
    expect(rowFor(after.stdout, issueAsk)).toContain(" · asked of bob · ");
    expect(rowFor(after.stdout, documentAsk)).toContain(" · asked of nobody · ");
    expect(printedCounts(after.stdout)).toEqual({ agent: 0, human: 2, total: 2 });
  } finally {
    await bob.close();
    rmSync(home, { force: true, recursive: true });
  }
});

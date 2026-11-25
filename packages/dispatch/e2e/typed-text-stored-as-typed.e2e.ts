/** Real stack: typing in the live document editor, whose typed text the server stores through the
 *  live write, which no stub serves. */
import { createIssue, createProject, getArtifactText } from "./api";
import { connectedDot, selectEditorText } from "./editor";
import { expect, test } from "./test";
import { asUser } from "./users";

// The typed text reaches the server inside the live write, so the markdown below is true within a
// round trip of the keystrokes.
const PROBE = { intervals: [50, 100, 200, 500, 1000, 2000] };

// A bold run a person types with `**` is closed where its closing `**` is typed, so the text typed
// after it is not bold and the stored markdown is what was typed (INFRA-4345 R6's editor half). At
// the base the bold took the rest of the line, `**Status quo is not an excuse. Reject historical
// mistakes.**`, and a person who turned it off after the space was left with a bold run ending in
// a space, stored `**Status quo is not an excuse.&#32;**&#82;eject` (dispatch://INFRA-1387).
test("typing a bold run closed before a space and a capital stores it as typed", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "## Notes\n\nPLACEHOLDER\n\nEnd of spec.\n",
    title: "Typed bold",
  });
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    await expect(connectedDot(page)).toHaveText("connected");
    await selectEditorText(page, "PLACEHOLDER");
    await page.keyboard.type("**Status quo is not an excuse.** Reject historical mistakes.");
    await expect
      .poll(
        () => getArtifactText(issue.primary_artifact_id).then(({ markdown }) => markdown),
        PROBE
      )
      .toContain("\n\n**Status quo is not an excuse.** Reject historical mistakes.\n\n");
    const { markdown } = await getArtifactText(issue.primary_artifact_id);
    expect(markdown).not.toContain("&#");
  } finally {
    await alice.close();
  }
});

/**
 * The reading list's walk (`e2e/reading-rows.ts`) over the pages whose text is
 * no record of the reference graph and still reads through the one renderer: a component's prose
 * on its project's Architecture page, a priority's description on the priority's own page, and
 * an issue's reason for touching no component, on the Architecture page's Not architectural list
 * and in the issue header's Components line. None of these texts is a `TextSource`, so a `#id`
 * in it names nothing, as each row of the list says (`cannotCarry`), but the five other forms a
 * text names a node by are read in it as every stored text reads them (R6): each draws as its
 * node's current name, the party as `@` and the session's current name, and none is left as
 * the characters written. Every host here is a page a reader reads, not a link or a button, so
 * each reference is a link there.
 *
 * Each row writes `readingText` where its page reads it from: into the component model file the
 * fake GitHub serves, whose body is the component's prose, and into the priority's description
 * and the issue's reason through the API.
 */
import { createPriority, patchIssue, putArchitectureSource, syncArchitectureSource } from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { expectReadsReferences, readingText, seedReadingWorld } from "./reading-rows";
import { COMPONENT_ID, COMPONENT_REPO, COMPONENT_TITLE, RENDERED_BODY } from "./reference-surfaces";
import { expect, test } from "./test";

/** CORE's architecture as reference-surfaces.ts seeds it: the fake GitHub serves the model and
 *  the source sync reads it in. The model file's body, under its front matter, is the
 *  component's prose. The Architecture page is drawn only for a project with a source, so every
 *  row that reads it seeds one. */
async function seedArchitecture(prose: string): Promise<void> {
  await seedFakeGithub({
    [COMPONENT_REPO]: {
      contents: "read",
      files: { [`${COMPONENT_ID}.md`]: `---\ntitle: ${COMPONENT_TITLE}\n---\n${prose}\n` },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: COMPONENT_REPO });
  const source = await syncArchitectureSource("CORE", COMPONENT_REPO);
  expect(source.last_error).toBeNull();
}

// SLOW(>=2s): the reading world, the fake GitHub's model read in by a source sync, and the
// Architecture page drawn with the component's details open.
test("component-prose: a component's prose draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The component's prose is the text the row reads.
  await seedArchitecture(readingText(world));

  await page.goto(`/projects/CORE/architecture?component=${COMPONENT_ID}`);
  const details = page.getByTestId("component-details");
  await expect(details.getByRole("heading", { level: 2, name: COMPONENT_TITLE })).toBeVisible();
  // The prose is the one rendered body of the details: the Code & definition section's text,
  // with the repository, the paths and the dependencies drawn beside it rather than in it.
  await expectReadsReferences(details.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world and the priority's own page drawn with its description.
test("priority-description: a priority's description draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  const priority = await createPriority({
    description: readingText(world),
    owner: "alice",
    title: "Keep one build",
  });

  await page.goto(`/priorities/${priority.id}`);
  // The group the description is drawn in for a person who may write it, which the default
  // context's alice is: the rendered text and the button that opens its editor, which is drawn
  // only while it holds the focus. The rendered text is no control, so its links stay links.
  const description = page.getByRole("group", { exact: true, name: "Description of P1" });
  await expectReadsReferences(description, world, "link");
});

// SLOW(>=2s): the reading world, the fake GitHub's model read in by a source sync, the host
// declared not architectural, and the Architecture page drawn with its Not architectural list
// open.
test("not-architectural-reason: a Not architectural row's reason draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  // The component's prose is words of its own, so the reason is the one stored text that names
  // the world's references.
  await seedArchitecture("The cache service.");
  await patchIssue(world.host.key, { components: { mode: "none", reason: readingText(world) } });

  await page.goto("/projects/CORE/architecture?view=none");
  const row = page
    .getByTestId("not-architectural-list")
    .getByTestId(`attachment-${world.host.key}`);
  // The row's head is a link to the issue drawn by the page itself; the reason under it is the
  // one rendered text of the row.
  await expectReadsReferences(row.locator(RENDERED_BODY), world, "link");
});

// SLOW(>=2s): the reading world, the host declared not architectural, and the host's page drawn
// with its header.
test("issue-components-reason: the issue header's Components line draws every form but a question id through the renderer", async ({
  page,
}) => {
  const world = await seedReadingWorld();
  await patchIssue(world.host.key, { components: { mode: "none", reason: readingText(world) } });

  await page.goto(`/issues/${world.host.key}`);
  const line = page.getByTestId("issue-components");
  await expect(line).toContainText("None —");
  await expectReadsReferences(line.locator(RENDERED_BODY), world, "link");
});

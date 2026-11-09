import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import { createIssue, createProject } from "./api";
import { expectParagraphReadings } from "./editor";
import { stubbed } from "./stub/stub-test";

// An asterisk beside a tilde opens or closes emphasis as the browser's engine (micromark) says:
// a tilde is punctuation, so `b*~*` emphasizes the tilde, while `~**b<*` and `*b**~~` pair
// nothing. One reader of that answer needs a browser and the rest do not: a past version hands
// the stored markdown to the live Milkdown editor (VersionView.tsx's `setMarkdown`), so this row
// is the editor's own parse of what the server stored, and it reads the document and its reading
// from the one fixture table. The spec editor binds to the Yjs fragment the server writes, which
// `fixtures.json` pins without a browser (`pm_json`, `yjs_update_v1_b64`); a comment parses
// through the headless engine, which `web/src/features/refs/fixture-table-readings.test.ts` runs
// over every document of that table; and the spelling the stored text takes for each run is
// `packages/envoy/internal/dispatch/pmdoc/writer_spelling_test.go`'s.
//
// What the row reads is the version the server already stored, so it runs against a recording
// rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer").

const agentSession = {
  actor: { kind: "session" as const, id: "e2e-emphasis-beside-a-tilde" },
  as: "agent" as const,
};

const entry = fixture("emphasis-beside-a-tilde");

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue(
    { project: "CORE", spec: entry.markdown, title: "Emphasis beside a tilde" },
    agentSession
  );
  return { issue: issue.key };
});

test("an asterisk beside a tilde emphasizes as the engine reads it in a past version of the spec @pr", async ({
  page,
  scene,
}) => {
  await page.goto(`/issues/${scene.issue}/artifacts/spec?v=1`);
  await expectParagraphReadings(page.getByTestId("version-view"), entry.reading.paragraphs);
});

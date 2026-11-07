import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import type { DocumentReading } from "@legion/proof-editor/reading";

import { createIssue, getArtifactText } from "./api";

/** Go's reading of stored markdown: written as a new issue's spec, parsed and rendered again. */
export async function goReadBack(markdown: string): Promise<string> {
  const issue = await createIssue({ project: "CORE", spec: markdown, title: "Read back" });
  return (await getArtifactText(issue.primary_artifact_id)).markdown;
}

/** The headless engine's reading of stored markdown, from engine-reading.ts. */
function engineReading(markdown: string): DocumentReading {
  const script = fileURLToPath(new URL("./engine-reading.ts", import.meta.url));
  return JSON.parse(execFileSync("bun", [script], { encoding: "utf8", input: markdown }));
}

/** The headless engine's reading of stored markdown: each table's rows, as their cells' text. */
export function engineTables(markdown: string): string[][][] {
  return engineReading(markdown).tables;
}

/** The headless engine's reading of each paragraph of stored markdown, as `readDocument` reads it
 *  (`@legion/proof-editor/reading`, `document-reading.ts:67`): the text of the DOM the serializer
 *  draws, with every run of whitespace collapsed to one space and the ends trimmed. What a `<br>`
 *  and a `hardbreak` each contribute to that text is `document-reading.ts:15-16`; what that does
 *  to a reading is the stored reading of `pmdoc/testdata/corpus/softbreak.md`, which reads
 *  `two spaces aloneare` where two spaces end the line and `a backslash keeps` where a tab stands
 *  before the backslash. The text of an inline `html` node and of a footnote reference's label,
 *  which the schema draws and no text node carries, is in it too. A row that holds Go's spelling
 *  of a body against the engine's, where the whitespace between its words is the point, reads the
 *  stored bytes (`goReadBack`) or `engineTables` instead; this answers what a reader sees. */
export function engineParagraphs(markdown: string): string[] {
  return engineReading(markdown).paragraphs.map(({ text }) => text);
}

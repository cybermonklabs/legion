import { useQuery } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { CitedBlock } from "./citation-label";

/** The question of the decision block `blockId` of `markdown` as the document holds it: the
 *  block's paragraphs before its options, with the marks its writer gave them, which the ask's
 *  own question - the block's plain text - leaves out. Undefined where the document holds no such
 *  block. */
export function blockQuestionMarkdown(
  markdown: string,
  blocks: readonly CitedBlock[],
  blockId: string
): string | undefined {
  const block = blocks.find((candidate) => candidate.id === blockId && candidate.type === "ask");
  if (block === undefined) return undefined;
  const lines = markdown.slice(block.from, block.to).split("\n");
  const body = lines.slice(1);
  const end = body.findIndex((line) => line.startsWith("- ") || line.startsWith(":::"));
  const question = (end < 0 ? body : body.slice(0, end)).join("\n").trim();
  return question === "" ? undefined : question;
}

/** The question of a decision block's ask with its writer's marks, for the Inbox's card and the
 *  margin's (INFRA-3231's R7, as the architect ruled): read from the block's document, which the
 *  page reads once for every card of it. Undefined for an ask that is no decision block, and
 *  until the document is read, when the card shows the ask's own question. */
export function useBlockQuestion(ask: {
  readonly block_id?: string | null;
  readonly block_artifact?: { readonly id: string } | null;
}): string | undefined {
  const artifactId = ask.block_artifact?.id;
  const blockId = ask.block_id ?? undefined;
  const text = useQuery({
    enabled: artifactId !== undefined && blockId !== undefined,
    queryFn: () => api.getArtifactTextWithBlocks(artifactId ?? ""),
    queryKey: ["artifact", artifactId, "text-with-blocks"],
  }).data;
  if (text === undefined || blockId === undefined) return undefined;
  return blockQuestionMarkdown(text.markdown, text.blocks ?? [], blockId);
}

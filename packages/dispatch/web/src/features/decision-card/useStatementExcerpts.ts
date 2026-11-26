import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { referenceTargetsQuery } from "../../api/queries";
import type { Decision } from "../../api/types";
import { EVENT_FRESH } from "../refs/reference-target";
import { blockWords, type StatementWords } from "./adapters";

/** What a statement the read has not answered for, or that nothing answers to now, is drawn by:
 *  no words, which the card draws as no line. */
const NO_WORDS: StatementWords = {};

/**
 * The words of each statement `decisions` shaped that their read holds none for, by its
 * reference: one batched read of the names of them all (`GET /api/v1/reference-targets`), which
 * a list of decision cards makes once, so no card reads a statement of its own
 * (`withStatementTexts`). A statement's words are its stored excerpt, a typed block's directive
 * left out (`blockWords`), with the server's reading of its text where the read sent one. The
 * read is kept fresh by the events that name its key, never by its age (`EVENT_FRESH`), and the
 * names in hand stand while a set that grew is read.
 */
export function useStatementExcerpts(
  decisions: readonly Decision[] | null | undefined
): (reference: string) => StatementWords {
  const references = useMemo(
    () =>
      (decisions ?? []).flatMap((decision) =>
        (decision.links ?? []).flatMap((link) =>
          link.to.block_id === undefined || link.to.text !== undefined || link.to.ref === undefined
            ? []
            : [link.to.ref]
        )
      ),
    [decisions]
  );
  const targets = useQuery({
    ...referenceTargetsQuery(references),
    ...EVENT_FRESH,
    placeholderData: keepPreviousData,
  }).data?.targets;
  return useMemo(() => {
    const named = new Map((targets ?? []).map((target) => [target.ref, target]));
    return (reference: string) => {
      const target = named.get(reference);
      return target === undefined || target.gone
        ? NO_WORDS
        : { text: blockWords(target.name), text_tree: target.text_tree };
    };
  }, [targets]);
}

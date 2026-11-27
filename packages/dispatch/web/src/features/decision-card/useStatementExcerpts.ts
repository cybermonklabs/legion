import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { referenceTargetsQuery } from "../../api/queries";
import type { Decision } from "../../api/types";
import type { QueryErrorProps } from "../../components/QueryError";
import { EVENT_FRESH } from "../refs/reference-target";
import { blockWords, type StatementWords } from "./adapters";

/** What a statement the read has not answered for, or that nothing answers to now, is drawn by:
 *  no words, which the card draws as its word for a statement it shaped
 *  (`DECISION_LINK_WORDS.statement`). */
const NO_WORDS: StatementWords = {};

/** The statements' names, as `useStatementExcerpts` hands them to the card's caller. */
export interface StatementExcerpts {
  /** The read's failure with its Retry (`QueryError`'s props but its message, which
   *  `StatementNamesError` words): both callers hand it to that one component, which draws it as
   *  the same failed read in the same words on each surface; undefined while the read has not
   *  failed. */
  readonly failure: Omit<QueryErrorProps, "message"> | undefined;
  /** The words of the statement `reference` names: none until the read answers for it, where it
   *  failed, or where the read names it gone. */
  readonly statementOf: (reference: string) => StatementWords;
}

/**
 * The words of each statement `decisions` shaped that their read holds none for, by its
 * reference: one batched read of the names of them all (`GET /api/v1/reference-targets`), which
 * a list of decision cards makes once, so no card reads a statement of its own
 * (`withStatementTexts`). A statement's words are its stored excerpt, a typed block's directive
 * left out (`blockWords`), with the server's reading of its text where the read sent one. The
 * read is kept fresh by the events that name its key, never by its age (`EVENT_FRESH`), and the
 * names in hand stand while a set that grew is read. A failed read is shown by the caller with
 * `StatementNamesError` (`failure`), and each statement it holds no words for is still drawn,
 * by the card's word for a statement it shaped.
 */
export function useStatementExcerpts(
  decisions: readonly Decision[] | null | undefined
): StatementExcerpts {
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
  const read = useQuery({
    ...referenceTargetsQuery(references),
    ...EVENT_FRESH,
    placeholderData: keepPreviousData,
  });
  const targets = read.data?.targets;
  const statementOf = useMemo(() => {
    const named = new Map((targets ?? []).map((target) => [target.ref, target]));
    return (reference: string) => {
      const target = named.get(reference);
      return target === undefined || target.gone
        ? NO_WORDS
        : { text: blockWords(target.name), text_tree: target.text_tree };
    };
  }, [targets]);
  return {
    failure: read.isError
      ? { error: read.error, onRetry: () => void read.refetch(), retrying: read.isFetching }
      : undefined,
    statementOf,
  };
}

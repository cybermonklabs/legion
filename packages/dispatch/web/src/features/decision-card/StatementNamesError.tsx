import type { ReactNode } from "react";

import { QueryError } from "../../components/QueryError";
import type { StatementExcerpts } from "./useStatementExcerpts";

/**
 * What a list of decision cards says where its one batched read of the statements' names failed
 * (`useStatementExcerpts`): the read's failure and its Retry, drawn as every failed read of the
 * app is (`QueryError`), and nothing when the read has not failed (`failure` undefined). The
 * Decisions tab and the margin's list both draw it, so the sentence is said in one place and in
 * the same words on both.
 *
 * The sentence is this component's and no word of the card's (`decision-card-words.ts`): the
 * card never draws it, and a word of that table is a word Dashboard, which takes the card's
 * words in, would say unasked.
 */
export function StatementNamesError({
  failure,
}: {
  failure: StatementExcerpts["failure"];
}): ReactNode {
  return failure === undefined ? null : (
    <QueryError message="Could not load the statements these decisions shaped." {...failure} />
  );
}

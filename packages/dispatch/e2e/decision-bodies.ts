// A decision block (`:::ask`) written into a text body draws a decision card nobody can answer
// where it stands, so the server refuses every body that holds one: 400 DECISION_BLOCK_IN_BODY
// on a comment, a reply, a message and a broadcast alike (INFRA-1201). A browser row that has
// to read such a card therefore writes the body straight into Postgres, the way a bulk thread
// is seeded (`e2e/inbox-threads.ts`): into `comments` and `messages`, never into an index table.
// The triggers of migrations 0142 to 0144 owe the row its block index themselves, and the card
// is drawn from the body's Markdown by the read-only engine, so no caller here writes an index
// row.
import { expect, type Locator } from "@playwright/test";

import type { Actor } from "../web/src/api/types";
import { authorLiteral } from "./inbox-threads";
import { sql } from "./psql";
import { sqlLiteral } from "./seed";

/** The one line the card draws under a decision block a reader meets in a body. */
export const TEXT_ONLY_LINE =
  "This question is text: it must be opened as an ask before anyone can answer it.";

/** A uuid column's value, or SQL null where the row carries none. */
function uuidLiteral(id: string | undefined): string {
  return id === undefined ? "null" : `${sqlLiteral(id)}::uuid`;
}

/**
 * One comment on `issueKey` by `author` whose `body` holds a decision block, written as one
 * `insert into comments`, and the new comment's id, which a row opens its thread by.
 *
 * `replyTo` is the head of the thread the comment hangs under, as the server stores it, and
 * `askId` the ask whose thread it sits in. A reply under an open ask records whose turn it is
 * as a write through the API would (`askReplyTurn`, `api/comment_create.go`): a human's reply
 * hands the turn to the agent, an agent's hands it back.
 */
export async function seedDecisionBody({
  askId,
  author,
  body,
  issueKey,
  replyTo,
}: {
  askId?: string;
  author: Actor;
  body: string;
  issueKey: string;
  replyTo?: string;
}): Promise<string> {
  const turn =
    askId === undefined ? "null" : sqlLiteral(author.kind === "user" ? "agent" : "human");
  // The insert stands in a CTE the statement selects from: psql prints an `INSERT 0 1` status
  // line under the rows an `insert ... returning` answers with, and a `select` is the id alone.
  const seeded = await sql(`
    with seeded as (
      insert into comments (issue_key, author, body, ask_id, reply_to, turn)
      values (${sqlLiteral(issueKey)}, ${authorLiteral(author)}, ${sqlLiteral(body)},
              ${uuidLiteral(askId)}, ${uuidLiteral(replyTo)}, ${turn})
      returning id
    )
    select id from seeded
  `);
  return seeded.trim();
}

/**
 * One message on `issueKey` by `author` whose `body` holds a decision block, written as one
 * `insert into messages`, and the new message's id, which the Conversation draws its turn by
 * (`data-turn="message:<id>"`). It is a root message of the issue, as a message written through
 * the API with no target and no `in_reply_to` is.
 */
export async function seedDecisionMessage({
  author,
  body,
  issueKey,
}: {
  author: Actor;
  body: string;
  issueKey: string;
}): Promise<string> {
  const seeded = await sql(`
    with seeded as (
      insert into messages (issue_key, author, body)
      values (${sqlLiteral(issueKey)}, ${authorLiteral(author)}, ${sqlLiteral(body)})
      returning id
    )
    select id from seeded
  `);
  return seeded.trim();
}

/**
 * Requires `scope` to draw exactly `count` decision cards and each of them to carry the one
 * line a card in a body draws, once, and nothing a reader could answer it with: no option
 * control and no Answer button. A row that finds no card fails on the count.
 */
export async function expectTextOnlyDecisions(scope: Locator, count: number): Promise<void> {
  const cards = scope.locator('section[data-proof-block-type="ask"]');
  await expect(cards).toHaveCount(count);
  for (let index = 0; index < count; index += 1) {
    const card = cards.nth(index);
    const line = card.locator("[data-dispatch-ask-text-only]");
    await expect(line).toHaveCount(1);
    await expect(line).toHaveText(TEXT_ONLY_LINE);
    await expect(card.getByRole("radio")).toHaveCount(0);
    await expect(card.getByRole("checkbox")).toHaveCount(0);
    await expect(card.getByRole("button", { exact: true, name: "Answer" })).toHaveCount(0);
  }
}

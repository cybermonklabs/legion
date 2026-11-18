import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { decode } from "@toon-format/toon";
import { renderInbound } from "../delivery";
import { sessionAnswerRelationPhrase } from "../session-answer-relations";

/**
 * The report's case (dispatch://INFRA-4344) as a real outbox published it: a person's reply in
 * the thread of session A's ask, on an issue with no route and no claim, reaches A, which asked;
 * C, which follows the ask; B, which wrote a version of the issue's spec; and the issue's own
 * topic. packages/envoy's TestAnAskReplyIsTheSessionsOwnOnlyForItsAskerAndFollowers drives the
 * case through the real API and outbox and writes each copy it was handed, with what the reader's
 * answers read calls the reply, into this fixture; that test's doc gives the fixture's shape. The
 * card is rendered here from those same bytes, so it and the answers read cannot tell one session
 * the same reply two ways.
 */
interface AskReplyFixture {
  readonly issue_key: string;
  readonly ask_id: string;
  readonly sessions: Readonly<Record<"A" | "B" | "C" | "D", string>>;
  readonly copies: readonly AskReplyCopy[];
}

interface AskReplyCopy {
  readonly reply: "first" | "third";
  readonly reader: "A" | "B" | "C" | null;
  readonly topic: string;
  readonly relation: string | null;
  readonly envelope: unknown;
}

/** A copy handed on a session's own topic, with what that session's answers read calls it. */
type RecordedCopy = AskReplyCopy & {
  readonly reader: NonNullable<AskReplyCopy["reader"]>;
  readonly relation: string;
};

type Card = { envoy: Record<string, unknown> & { dispatch: Record<string, unknown> } };

const fixture = JSON.parse(
  readFileSync(
    new URL("../../../contracts/fixtures/ask-reply-recipients.json", import.meta.url),
    "utf8"
  )
) as AskReplyFixture;

const { A, B, C } = fixture.sessions;
/** A session that subscribes to the issue and neither asked the ask nor follows it. */
const E = "session-e";

/** The copy of `reply` handed on `reader`'s own topic, or on the issue's topic for null. */
function copyOf(reply: AskReplyCopy["reply"], reader: AskReplyCopy["reader"]): AskReplyCopy {
  const found = fixture.copies.find((copy) => copy.reply === reply && copy.reader === reader);
  if (found === undefined) throw new Error(`the fixture holds no ${reply} reply for ${reader}`);
  return found;
}

/** `copy` as the card `session` reads, from the topic it was handed on. */
function cardOf(copy: AskReplyCopy, session: string): Card {
  return decode(renderInbound(JSON.stringify(copy.envelope), session, copy.topic).content) as Card;
}

/** The card of a reply on the reader's own ask: the command that answers the ask, and whose turn
 *  it is. */
function expectOwnersCard(card: Card): void {
  expect({
    relation: card.envoy.dispatch.relation,
    reply_with: card.envoy.reply_with,
    waiting_on: card.envoy.dispatch.waiting_on,
  }).toEqual({
    relation: "replied on your ask",
    reply_with: {
      command: `dispatch comment --issue ${fixture.issue_key} --body ... --reply-to-ask ${fixture.ask_id}`,
    },
    waiting_on: "agent",
  });
}

/** The card of a reply that reached a session whose ask it is not: why it has the reply, and no
 *  command that answers the ask and no turn. */
function expectOthersCard(card: Card): void {
  expect({
    relation: card.envoy.dispatch.relation,
    reply_with: card.envoy.reply_with,
    waiting_on: card.envoy.dispatch.waiting_on,
  }).toEqual({ relation: "commented on an issue", reply_with: undefined, waiting_on: undefined });
}

describe("a person's reply on A's ask, as each session's card reads the copy it was handed", () => {
  test("A, which asked, and C, which follows, read a reply on their own ask on their own topics", () => {
    expectOwnersCard(cardOf(copyOf("first", "A"), A));
    expectOwnersCard(cardOf(copyOf("first", "C"), C));
  });

  test("A reads the reply that reached it after it left its ask as a reply on its own ask", () => {
    expectOwnersCard(cardOf(copyOf("third", "A"), A));
  });

  test("B, credited on the issue's spec, reads a comment on an issue, as its answers read does", () => {
    const copy = copyOf("first", "B");
    const card = cardOf(copy, B);

    expectOthersCard(card);
    expect(copy.relation).not.toBeNull();
    expect(card.envoy.dispatch.relation).toBe(sessionAnswerRelationPhrase(copy.relation ?? ""));
    // Which ask the reply is on, and what it says, stay on the card.
    expect(card.envoy.re).toBe(`dispatch://${fixture.issue_key}/ask/${fixture.ask_id}`);
    expect(card.envoy.dispatch.question).toBe("Which database backs the queue?");
    expect(card.envoy.dispatch.reply).toBe("Which pool size do you need?");
    expect(card.envoy.dispatch.state).toBe("open");
  });

  test("a subscriber of the issue reads a comment on an issue, and A, which also follows the issue, its own ask", () => {
    const copy = copyOf("first", null);

    expectOthersCard(cardOf(copy, E));
    expectOwnersCard(cardOf(copy, A));
  });

  test("every copy's card names the relation the reader's answers read gives the same reply", () => {
    const recorded = fixture.copies.filter(
      (copy): copy is RecordedCopy => copy.reader !== null && copy.relation !== null
    );
    expect(recorded.map((copy) => `${copy.reply} ${copy.reader}`)).toEqual([
      "first A",
      "first B",
      "first C",
      "third A",
    ]);
    for (const copy of recorded) {
      expect(cardOf(copy, fixture.sessions[copy.reader]).envoy.dispatch.relation).toBe(
        sessionAnswerRelationPhrase(copy.relation)
      );
    }
  });
});

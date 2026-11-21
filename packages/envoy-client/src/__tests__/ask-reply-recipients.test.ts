import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { decode } from "@toon-format/toon";
import { renderInbound } from "../delivery";
import { sessionAnswerRelationPhrase } from "../session-answer-relations";
import { splitWordsBlocks } from "../words-block";

/**
 * The report's case (dispatch://INFRA-4344) as a real outbox published it: a person's reply in
 * the thread of session A's ask, on an issue with no route and no claim, reaches A, which asked;
 * C, which follows the ask; B, which wrote a version of the issue's spec; and the issue's own
 * topic. packages/envoy's TestAnAskReplyIsTheSessionsOwnOnlyForItsAskerAndFollowers drives the
 * case through the real API and outbox and writes each copy it was handed, with what the reader's
 * answers read calls the reply, into this fixture; that test's doc gives the fixture's shape. The
 * card is rendered here from those same bytes, and no session here starts or stops owning the ask
 * between a reply and those reads, so the card and the answers read agree. The fixture's
 * `stand_in` holds the same case for a reply too large for the bus, as the outbox's stand-ins.
 */
interface AskReplyFixture {
  readonly issue_key: string;
  readonly ask_id: string;
  readonly sessions: Readonly<Record<"A" | "B" | "C" | "D", string>>;
  readonly copies: readonly AskReplyCopy[];
  readonly stand_in: StandInCase;
}

interface AskReplyCopy {
  readonly reply: "first" | "third";
  readonly reader: "A" | "B" | "C" | null;
  readonly topic: string;
  readonly relation: string | null;
  readonly envelope: unknown;
}

/** A person's reply too large for the bus, in the thread of an ask seven sessions own: the
 *  stand-in (`payload_omitted`) the outbox sent each owner and B in its place. */
interface StandInCase {
  readonly ask_id: string;
  /** The ask's `ask_sessions`: the asker, then each follower in the order it joined. */
  readonly owners: readonly string[];
  readonly copies: readonly StandInCopy[];
}

/** One stand-in, handed on the own topic of `reader`, a session id, with what that session's
 *  answers read calls the reply. */
interface StandInCopy {
  readonly reader: string;
  readonly topic: string;
  readonly relation: string;
  readonly envelope: { readonly payload: string };
}

/** A copy handed on a session's own topic, with what that session's answers read calls it. */
type RecordedCopy = AskReplyCopy & {
  readonly reader: NonNullable<AskReplyCopy["reader"]>;
  readonly relation: string;
};

/** A card's fields, as TOON's `decode` reads them once its blocks are split off, and the words of
 *  its blocks by the name of the field that holds them. */
type Card = {
  envoy: Record<string, unknown> & { dispatch: Record<string, unknown> };
  words: Readonly<Record<string, string>>;
};

/** The card `content` as a reader reads it. */
function read(content: string): Card {
  const { head, blocks } = splitWordsBlocks(content);
  const { envoy } = decode(head) as Pick<Card, "envoy">;
  return { envoy, words: Object.fromEntries(blocks.map(({ name, text }) => [name, text])) };
}

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
  return read(renderInbound(JSON.stringify(copy.envelope), session, copy.topic).content);
}

/** The card of a reply on the reader's own ask, `askID` (A's unless named): the command that
 *  answers the ask, and whose turn it is. */
function expectOwnersCard(card: Card, askID = fixture.ask_id): void {
  expect({
    relation: card.envoy.dispatch.relation,
    reply_with: card.envoy.reply_with,
    waiting_on: card.envoy.dispatch.waiting_on,
  }).toEqual({
    relation: "replied on your ask",
    reply_with: {
      command: `dispatch comment --issue ${fixture.issue_key} --body ... --reply-to-ask ${askID}`,
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
    // Which ask the reply is on, and what it says, stay on the card.
    expect(card.envoy.re).toBe(`dispatch://${fixture.issue_key}/ask/${fixture.ask_id}`);
    expect(card.envoy.dispatch.question).toBe("Which database backs the queue?");
    expect(card.words["reply"]).toBe("Which pool size do you need?");
    expect(card.envoy.dispatch.state).toBe("open");
  });

  test("a subscriber of the issue reads a comment on an issue, and A, which also subscribes to the issue, its own ask", () => {
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

describe("a reply too large for the bus, on an ask seven sessions own, as each session's card reads the stand-in it was handed", () => {
  const standIn = fixture.stand_in;
  /** The reply's whole body, as a host reads it from the comment route once the stand-in named
   *  the comment cut (`renderInbound`'s `cutItem`), and renders the stand-in again with it. */
  const wholeBody = "Ship to the region whose queue is nearest. ".repeat(150);

  /** The stand-in handed on `reader`'s own topic. */
  function standInOf(reader: string): StandInCopy {
    const found = standIn.copies.find((copy) => copy.reader === reader);
    if (found === undefined) throw new Error(`the fixture holds no stand-in for ${reader}`);
    return found;
  }

  /** `copy` as its reader's card: as it arrived, then rendered again with the body read whole. */
  function standInCards(copy: StandInCopy): [Card, Card] {
    const raw = JSON.stringify(copy.envelope);
    return [
      read(renderInbound(raw, copy.reader, copy.topic).content),
      read(renderInbound(raw, copy.reader, copy.topic, wholeBody).content),
    ];
  }

  test("each copy is a stand-in, with no body, that names the ask's seven owners whole and in order", () => {
    expect(standIn.owners).toHaveLength(7);
    expect(standIn.copies.map((copy) => copy.reader)).toEqual([...standIn.owners, B]);
    for (const copy of standIn.copies) {
      const { payload } = JSON.parse(copy.envelope.payload) as {
        payload: Record<string, unknown>;
      };
      expect({
        payload_omitted: payload["payload_omitted"],
        body: payload["body"],
        ask_sessions: payload["ask_sessions"],
      }).toEqual({ payload_omitted: true, body: undefined, ask_sessions: [...standIn.owners] });
    }
  });

  test("each owner reads a reply on its own ask on its own topic, before and after its body is read whole", () => {
    for (const owner of standIn.owners) {
      const [arrived, whole] = standInCards(standInOf(owner));

      expectOwnersCard(arrived, standIn.ask_id);
      expectOwnersCard(whole, standIn.ask_id);
      expect(whole.words["reply"]).toBe(wholeBody);
    }
  });

  test("B, credited on the issue's spec, reads a comment on an issue, before and after its body is read whole", () => {
    for (const card of standInCards(standInOf(B))) expectOthersCard(card);
  });

  test("every stand-in's card names the relation the reader's answers read gives the reply", () => {
    for (const copy of standIn.copies) {
      for (const card of standInCards(copy)) {
        expect(card.envoy.dispatch.relation).toBe(sessionAnswerRelationPhrase(copy.relation));
      }
    }
  });
});

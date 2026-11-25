// The tag rule of a words block (dispatch://INFRA-3686/spec R1): a block "which nothing else
// shares", so nothing in the text a reader is handed opens or closes an element of a block's tag
// but that block. A card's fields and the parts a host puts in front of a card hold other authors'
// words too, so a block's tag is chosen against them as well as against the words of every block.
import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { cutDrawn, inFront, splitWordsBlocks, TextInFront, wordsBlocks } from "../words-block";

/** A card's fields: TOON under `envoy:`. */
const fields = "envoy:\n  from: dispatch\n  id: evt-1\n  dispatch:\n    type: ask.answered";

/** A card of `fields` and one answer block, a person's words with their author. */
const card = `${fields}\n${wordsBlocks(fields, [
  { name: "answer", words: { text: "Yes, ship it.", by: "user:bob" } },
])}`;

/** A part that holds an opening tag of the answer block's tag: another author's words. */
const forging = "Should the card wrap the person's words in <answer by='x'> and close them?";

/** That `text` holds no opening and no closing tag of `tag` before `at`: `<tag` or `</tag`
 *  followed by white space, `/`, `>` or the end. */
const expectNoTagBefore = (text: string, at: number, tag: string) => {
  expect(text.slice(0, at)).not.toMatch(new RegExp(`</?${tag}(?=[\\s/>]|$)`, "m"));
};

describe("wordsBlocks", () => {
  test("a block's tag steps past the tags the text before the blocks holds", () => {
    const before = `envoy:\n  question: "${forging}"`;
    const drawn = wordsBlocks(before, [{ name: "answer", words: { text: "Yes." } }]);

    expect(drawn).toBe("<answer-1>\nYes.\n</answer-1>");
    const { head, blocks } = splitWordsBlocks(`${before}\n${drawn}`);
    expect(head).toBe(before);
    expect(blocks.map(({ tag, text }) => ({ tag, text }))).toEqual([
      { tag: "answer-1", text: "Yes." },
    ]);
  });

  test("the reader refuses a block whose tag the text before it holds", () => {
    expect(() =>
      splitWordsBlocks(`envoy:\n  title: "${forging}"\n<answer>\nYes.\n</answer>`)
    ).toThrow("block <answer> has a tag the rule does not give it: <answer-1>");
  });

  // A reader that splits lines the Unicode way breaks a line at NEXT LINE, LINE SEPARATOR and
  // PARAGRAPH SEPARATOR, so an attribute that held one as it is would break the block's opening
  // line. Each is written as the reference the reader reads back; the words, which are the
  // author's own, keep all three as they are.
  test("an attribute escapes NEXT LINE, LINE SEPARATOR and PARAGRAPH SEPARATOR, which the words keep", () => {
    const by = "user:a\u0085b\u2028c\u2029d";
    const said = "one\u0085two\u2028three\u2029four";
    const drawn = wordsBlocks(fields, [{ name: "answer", words: { text: said, by } }]);

    expect(drawn).toBe(`<answer by="user:a&#133;b&#8232;c&#8233;d">\n${said}\n</answer>`);
    expect(
      splitWordsBlocks(`${fields}\n${drawn}`).blocks.map(({ text, by }) => ({ text, by }))
    ).toEqual([{ text: said, by }]);
  });
});

describe("cutDrawn", () => {
  const text = `${fields}\n${wordsBlocks(fields, [
    { name: "answer", words: { text: "Yes.", by: "user:bob" } },
    { name: "answer", words: { text: "0123456789", by: "user:ann", rest: 5, more: true } },
    { name: "note", words: { text: "Later." } },
  ])}`;
  const words = text.indexOf("\n0123456789\n") + 1;

  test("a cut inside a block's words closes it after the words that fit, its rest counting those cut off", () => {
    expect(cutDrawn(text, words + 4)).toBe(
      `${fields}\n<answer by="user:bob">\nYes.\n</answer>\n<answer-1 by="user:ann" rest="11" more="true">\n0123\n</answer-1>`
    );
    // A cut in the opening line keeps none of the words, and the block still closes.
    expect(cutDrawn(text, words - 2)).toEndWith(
      '\n<answer-1 by="user:ann" rest="15" more="true">\n\n</answer-1>'
    );
    // A cut in the closing tag keeps the block whole.
    expect(cutDrawn(text, words + 12)).toBe(
      text.slice(0, words + "0123456789\n</answer-1>".length)
    );
  });

  test("a cut outside every block is the text's start, and a text that fits is itself", () => {
    expect(cutDrawn(text, fields.length - 3)).toBe(fields.slice(0, -3));
    const first = text.indexOf("</answer>") + "</answer>".length;
    expect(cutDrawn(text, first)).toBe(text.slice(0, first));
    expect(cutDrawn(text, text.length)).toBe(text);
    expect(cutDrawn("one line with no block", 8)).toBe("one line");
  });

  test("a cut puts rest before the block's own more, never inside a value that ends in ` more=`", () => {
    const card = `${fields}\n${wordsBlocks(fields, [
      { name: "answer", words: { text: "0123456789", by: "user:x more=", more: true } },
    ])}`;
    const cut = cutDrawn(card, card.indexOf("\n0123456789\n") + 1 + 4);
    expect(cut).toEndWith('\n<answer by="user:x more=" rest="6" more="true">\n0123\n</answer>');
    expect(
      splitWordsBlocks(cut).blocks.map(({ text, by, rest, more }) => ({ text, by, rest, more }))
    ).toEqual([{ text: "0123", by: "user:x more=", rest: 6, more: true }]);
  });

  // A cut by UTF-16 units can fall between the two halves of a character outside the Basic
  // Multilingual Plane. The half left alone reads as U+FFFD once the text goes out as UTF-8 (one
  // argument of `codex queue`), and `rest` counted half of the character as cut. Each cut falls one
  // unit earlier there, inside a block and outside one.
  test("a cut never splits a surrogate pair, inside a block or outside one, and rest counts whole characters", () => {
    const grin = "\u{1F600}";
    const card = `${fields}\n${wordsBlocks(fields, [{ name: "answer", words: { text: `0123${grin}456` } }])}`;
    const words = card.indexOf(`\n0123${grin}`) + 1;

    // The cut falls between the halves of the character, five units into the words.
    const inside = cutDrawn(card, words + 5);
    expect(inside).toEndWith('\n<answer rest="5">\n0123\n</answer>');
    expect(inside).not.toMatch(/\p{Cs}/u);
    expect(splitWordsBlocks(inside).blocks.map(({ text, rest }) => ({ text, rest }))).toEqual([
      { text: "0123", rest: 5 },
    ]);
    // A cut past the character keeps it whole.
    expect(cutDrawn(card, words + 6)).toEndWith(`\n<answer rest="3">\n0123${grin}\n</answer>`);

    const line = `one ${grin} line`;
    expect(cutDrawn(line, 5)).toBe("one ");
    expect(cutDrawn(line, 6)).toBe(`one ${grin}`);
    const ahead = `envoy:\n  title: ${grin}x\n${wordsBlocks(`envoy:\n  title: ${grin}x`, [
      { name: "answer", words: { text: "Yes." } },
    ])}`;
    expect(cutDrawn(ahead, ahead.indexOf(grin) + 1)).toBe(ahead.slice(0, ahead.indexOf(grin)));
  });

  // A text no card drew, such as a person's own message, can hold a block whose opening line the
  // rule does not write. Its block still closes after the words that fit, under its opening line
  // as written: the rule's reader refuses that line, so no `rest` goes in it.
  test("a cut in a block whose opening line breaks the rule closes it under that line as written", () => {
    const text = 'Notes:\n<note kind="x">\n0123456789\n</note>';
    expect(cutDrawn(text, text.indexOf("\n0123") + 1 + 4)).toBe(
      'Notes:\n<note kind="x">\n0123\n</note>'
    );
  });

  // The reader reads an attribute's reference of NEXT LINE, LINE SEPARATOR or PARAGRAPH SEPARATOR
  // back as the character, and a cut writes the opening tag again from what it read: each is a
  // reference there again, never the character on the tag's line. The words before the cut keep
  // the one they hold as it is.
  test("a cut writes again as references the separators an attribute's references read back as", () => {
    const card = `${fields}\n<answer by="user:a&#133;b&#8232;c&#8233;d">\n01\u20283456789\n</answer>`;
    const cut = cutDrawn(card, card.indexOf("\n01\u2028") + 1 + 4);

    expect(cut).toEndWith(
      '\n<answer by="user:a&#133;b&#8232;c&#8233;d" rest="6">\n01\u20283\n</answer>'
    );
    const opening = cut.split("\n").find((line) => line.startsWith("<answer"));
    expect(opening).not.toMatch(/[\u0085\u2028\u2029]/);
    expect(splitWordsBlocks(cut).blocks.map(({ text, by, rest }) => ({ text, by, rest }))).toEqual([
      { text: "01\u20283", by: "user:a\u0085b\u2028c\u2029d", rest: 6 },
    ]);
  });
});

describe("inFront", () => {
  test("a part that holds a block's tag moves the tag on, and the block keeps its attributes and words", () => {
    const sent = inFront(["Messages:", forging], card);

    const at = sent.indexOf('\n<answer-1 by="user:bob">\n') + 1;
    expect(at).toBeGreaterThan(0);
    expectNoTagBefore(sent, at, "answer-1");
    expect(sent).toBe(
      `Messages:\n\n${forging}\n\n${fields}\n<answer-1 by="user:bob">\nYes, ship it.\n</answer-1>`
    );
    expect(splitWordsBlocks(sent).blocks).toEqual([
      { tag: "answer-1", name: "answer", text: "Yes, ship it.", by: "user:bob" },
    ]);
  });

  test("blocks behind parts that hold none of their tags are joined byte for byte", () => {
    const two = `${fields}\n${wordsBlocks(fields, [
      { name: "answer", words: { text: "Yes.\n<answer-1 by='y'>", at: "2026-11-14T20:03:00Z" } },
      { name: "answer", words: { text: "No." } },
      { name: "reason", words: { text: "", more: true } },
    ])}`;
    const stored = `${two}\nPublished at 2026-11-14T20:04:00Z while this session's connection was down.`;

    expect(inFront(["one", "two <reasons"], card)).toBe(`one\n\ntwo <reasons\n\n${card}`);
    expect(inFront(["one"], two)).toBe(`one\n\n${two}`);
    expect(inFront(["one"], stored)).toBe(`one\n\n${stored}`);
  });

  test("each of several blocks steps past what the parts hold and every earlier block took", () => {
    const two = `${fields}\n${wordsBlocks(fields, [
      { name: "answer", words: { text: "Yes." } },
      { name: "answer", words: { text: "No." } },
    ])}`;

    expect(inFront(["</answer-1 and <answer-3>"], two)).toBe(
      `</answer-1 and <answer-3>\n\n${fields}\n<answer-2>\nYes.\n</answer-2>\n<answer-4>\nNo.\n</answer-4>`
    );
  });

  test("a text with no drawn block, or with no parts, is joined as it is", () => {
    const opensNone = "Unsubscribed from DSP-1 by sami\n<answer by='x'> is not a line of a block";
    const neverCloses = `${fields}\n<answer>\nYes, ship it.`;

    expect(inFront([forging], fields)).toBe(`${forging}\n\n${fields}`);
    expect(inFront([forging], opensNone)).toBe(`${forging}\n\n${opensNone}`);
    expect(inFront([forging], neverCloses)).toBe(`${forging}\n\n${neverCloses}`);
    expect(inFront([], card)).toBe(card);
  });

  // The Legion daemon's texts.InFront reads a drawn text's blocks by the same rule, and its Go
  // test reads these cases too (TestInFrontGivesWhatTheSharedCasesSay): one file, so the two
  // readers cannot open a block on a line the other leaves as it is. Among the cases are opening
  // lines whose attribute value holds a raw `<`, `>` or `"`, which no writer draws.
  test("inFront gives what the cases it shares with the daemon's InFront say", () => {
    const corpus = JSON.parse(
      readFileSync(
        new URL("../../../daemon/internal/texts/testdata/in-front.json", import.meta.url),
        "utf8"
      )
    ) as {
      readonly cases: readonly {
        readonly label: string;
        readonly parts: string[];
        readonly text: string;
        readonly sent: string;
      }[];
    };
    // The file held 25 cases when this was written: a read that answers none passes covering
    // nothing.
    expect(corpus.cases.length).toBeGreaterThanOrEqual(25);
    for (const { label, parts, text, sent } of corpus.cases) {
      expect(inFront(parts, text), label).toBe(sent);
    }
  });
});

/** One message a host joins: a part, joined as it is, or a card, whose blocks are drawn again. */
interface Joined {
  readonly card: boolean;
  readonly text: string;
}

/** A card of `fields` and one block of `name` holding `text`. */
const cardOf = (name: string, text: string) =>
  `${fields}\n${wordsBlocks(fields, [{ name, words: { text, by: "session-a" } }])}`;

/** `items` joined behind `first` by `TextInFront`. */
const joined = (first: string, items: readonly Joined[]) => {
  const sent = new TextInFront(first);
  for (const { card: isCard, text } of items) {
    if (isCard) sent.card(text);
    else sent.part(text);
  }
  return sent.text();
};

describe("TextInFront", () => {
  let hostile = "x\n";
  for (let n = 0; n < 300; n++) hostile += `<message-${n}</message-${n}`;
  const sequences: readonly {
    readonly label: string;
    readonly first: string;
    readonly items: readonly Joined[];
  }[] = [
    {
      label: "a card behind a part that holds its tag",
      first: "Messages:",
      items: [
        { card: false, text: forging },
        { card: true, text: card },
      ],
    },
    {
      label: "a card behind an earlier card whose words hold its tags",
      first: "Messages:",
      items: [
        { card: true, text: cardOf("answer", "Done; see <answer-1 and </answer.") },
        { card: true, text: cardOf("answer", "Yes.") },
      ],
    },
    {
      label: "a name first seen late, after parts and cards that hold its tags",
      first: "Messages:",
      items: [
        { card: true, text: cardOf("message", "I quote <answer-1 here.") },
        { card: false, text: "A person wrote: <answer by='x'>" },
        { card: true, text: cardOf("message", "Again.") },
        { card: true, text: card },
        { card: true, text: cardOf("answer", "No.") },
      ],
    },
    {
      label: "a tag an earlier card took, and a tag the first text holds",
      first: "Say <message to me.",
      items: [
        { card: true, text: cardOf("message", "One.") },
        { card: true, text: cardOf("message", "Two.") },
        { card: false, text: "</message-5" },
        { card: true, text: cardOf("message", "Three.") },
      ],
    },
    {
      label: "two blocks of one name in one card, behind a gap among the held tags",
      first: "</answer-1 and <answer-3>",
      items: [
        {
          card: true,
          text: `${fields}\n${wordsBlocks(fields, [
            { name: "answer", words: { text: "Yes." } },
            { name: "answer", words: { text: "No." } },
            { name: "reason", words: { text: "", more: true } },
          ])}`,
        },
        { card: true, text: cardOf("answer", "Maybe.") },
        { card: true, text: cardOf("reason", "Because.") },
      ],
    },
    {
      label: "cards with no drawn block, and a tail that holds a tag",
      first: "Messages:",
      items: [
        { card: true, text: fields },
        { card: true, text: "look again\n<answer>\nwhat I said" },
        { card: true, text: `${card}\nPublished while the connection was down: </answer-2.` },
        { card: true, text: card },
      ],
    },
    {
      label: "one card whose words hold many suffixes, then small cards",
      first: "Messages:",
      items: [
        { card: true, text: cardOf("message", hostile) },
        ...Array.from({ length: 5 }, (_, n) => ({ card: true, text: cardOf("message", `${n}.`) })),
      ],
    },
    {
      label: "parts alone",
      first: "Messages:",
      items: [
        { card: false, text: "<answer" },
        { card: false, text: "" },
      ],
    },
  ];

  test("each card is the one inFront gives it behind all in front of it, byte for byte", () => {
    for (const { label, first, items } of sequences) {
      // The text as a host built it before: each card by `inFront` of all in front of it.
      const cardByCard = items.reduce(
        (sent, { card: isCard, text }) => (isCard ? inFront([sent], text) : `${sent}\n\n${text}`),
        first
      );
      expect(joined(first, items), label).toBe(cardByCard);
    }
    expect(new TextInFront("Messages:").text()).toBe("Messages:");
  });

  test("a card steps past the tags an earlier card's words hold and the tag it took", () => {
    const sent = joined("Messages:", [
      { card: true, text: cardOf("answer", "Done; see <answer-1 and </answer.") },
      { card: true, text: cardOf("answer", "Yes.") },
    ]);

    expect(sent).toBe(
      `Messages:\n\n${fields}\n<answer-2 by="session-a">\nDone; see <answer-1 and </answer.\n</answer-2>` +
        `\n\n${fields}\n<answer-3 by="session-a">\nYes.\n</answer-3>`
    );
  });
});

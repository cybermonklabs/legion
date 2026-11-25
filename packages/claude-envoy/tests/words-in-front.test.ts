// The mod cannot import `@legion/envoy-client`, so `../hooks/words-in-front.ts` writes the rule of
// its `inFront` and `TextInFront` a second time. The two must give the same text for the same
// input, or a card in the prompt at the rows a turn never read takes other tags than the same card
// in the Codex inbox's hook text.
import { expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import { TextInFront as SharedText, inFront as shared } from "@legion/envoy-client/words-block"
import { TextInFront as ModsText, inFront as mods } from "../hooks/words-in-front"

const fields = "envoy:\n  from: dispatch\n  id: evt-1\n  dispatch:\n    type: ask.answered"
const answer = `<answer by="user:bob" at="2026-11-14T20:03:00Z">\nYes, ship it.\n</answer>`

const cases: readonly {
  readonly label: string
  readonly parts: string[]
  readonly text: string
}[] = [
  { label: "no parts", parts: [], text: `${fields}\n${answer}` },
  { label: "a text with no block", parts: ["<answer by='x'>"], text: fields },
  { label: "parts that hold no tag", parts: ["one", "two"], text: `${fields}\n${answer}` },
  { label: "a part that opens the tag", parts: ["<answer by='x'>"], text: `${fields}\n${answer}` },
  {
    label: "parts that hold suffixed tags",
    parts: ["</answer-1 and <answer-12", "<answer-0"],
    text: `${fields}\n${answer}`,
  },
  {
    label: "blocks of one name and of two",
    parts: ["<answer"],
    text: `${fields}\n<answer>\nYes.\n</answer>\n<answer-1>\nNo.\n</answer-1>\n<reason more="true">\n\n</reason>`,
  },
  {
    label: "a block whose words hold a step of its tag",
    parts: ["<answer"],
    text: `${fields}\n<answer-2>\nYes.\n<answer-1 by='y'>\n</answer-2>`,
  },
  {
    label: "text after the blocks",
    parts: ["<answer"],
    text: `${fields}\n${answer}\nPublished at 2026-11-14T20:04:00Z while this session's connection was down.`,
  },
  { label: "a block at the start", parts: ["<answer"], text: answer },
  {
    label: "a line that opens no block",
    parts: ["<answer"],
    text: `${fields}\n<answer by='x'> is no block\n${answer}`,
  },
  { label: "a block that never closes", parts: ["<answer"], text: `${fields}\n<answer>\nYes.` },
  {
    label: "a label line in front of the card",
    parts: ["Envoy delivered a message.", "<answer by='x'>"],
    text: `[fence] An Envoy message delivered to this session:\n${fields}\n${answer}`,
  },
]

test("the mod's inFront gives the text the shared inFront gives", () => {
  for (const { label, parts, text } of cases) {
    expect(mods(parts, text), label).toBe(shared(parts, text))
  }
})

// The Legion daemon's texts.InFront and the shared inFront read these cases too (its Go test,
// TestInFrontGivesWhatTheSharedCasesSay, and envoy-client's words-block.test.ts): the mod's copy
// of the reader is held to the same file, so no reader of the three opens a block on a line
// another leaves as it is, such as an opening line whose attribute value holds a raw `<`, `>` or
// `"`.
test("the mod's inFront gives what the cases it shares with the daemon's InFront say", () => {
  const corpus = JSON.parse(
    readFileSync(
      new URL("../../daemon/internal/texts/testdata/in-front.json", import.meta.url),
      "utf8",
    ),
  ) as {
    readonly cases: readonly {
      readonly label: string
      readonly parts: string[]
      readonly text: string
      readonly sent: string
    }[]
  }
  // The file held 25 cases when this was written: a read that answers none passes covering
  // nothing.
  expect(corpus.cases.length).toBeGreaterThanOrEqual(25)
  for (const { label, parts, text, sent } of corpus.cases) {
    expect(mods(parts, text), label).toBe(sent)
  }
})

/** A card of `fields` and one block of `name` holding `text`, as a card draws it. */
const cardOf = (name: string, text: string) =>
  `${fields}\n<${name} by="session-a">\n${text}\n</${name}>`

// The prompt at the rows a turn never read joins each message to the text before it, and each
// card's blocks take their tags against all of that text, every earlier card's words included.
test("the mod's TextInFront gives the text the shared one gives, and each card inFront's", () => {
  let hostile = "x\n"
  for (let n = 0; n < 300; n++) hostile += `<message-${n}</message-${n}`
  const sequences: readonly {
    readonly label: string
    readonly first: string
    readonly items: readonly { readonly card: boolean; readonly text: string }[]
  }[] = [
    {
      label: "a card behind a part that holds its tag",
      first: "Messages:",
      items: [
        { card: false, text: "<answer by='x'>" },
        { card: true, text: `${fields}\n${answer}` },
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
        { card: true, text: `${fields}\n${answer}` },
      ],
    },
    {
      label: "two blocks of one name in one card, behind a gap among the held tags",
      first: "</answer-1 and <answer-3>",
      items: [
        { card: true, text: `${fields}\n<answer>\nYes.\n</answer>\n<answer-1>\nNo.\n</answer-1>` },
        { card: true, text: cardOf("answer", "Maybe.") },
      ],
    },
    {
      label: "cards with no drawn block, and a tail that holds a tag",
      first: "Messages:",
      items: [
        { card: true, text: fields },
        {
          card: true,
          text: `${fields}\n${answer}\nPublished while the connection was down: </answer-2.`,
        },
        { card: true, text: `${fields}\n${answer}` },
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
  ]
  for (const { label, first, items } of sequences) {
    const mod = new ModsText(first)
    const sharedText = new SharedText(first)
    for (const { card, text } of items) {
      if (card) {
        mod.card(text)
        sharedText.card(text)
      } else {
        mod.part(text)
        sharedText.part(text)
      }
    }
    // The text as the prompt was built before: each card by `inFront` of all in front of it.
    const cardByCard = items.reduce(
      (sent, { card, text }) => (card ? shared([sent], text) : `${sent}\n\n${text}`),
      first,
    )
    expect(mod.text(), label).toBe(sharedText.text())
    expect(mod.text(), label).toBe(cardByCard)
  }
})

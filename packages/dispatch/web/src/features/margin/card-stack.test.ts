import { expect, test } from "bun:test";
import { render } from "@testing-library/react";
import { createElement, type ReactNode, useCallback } from "react";

import { CardStackFocus, focusTops, useCardStack } from "./card-stack";

/**
 * The stack of one region of the margin (`useCardStack`) and its focus rule (`focusTops`,
 * `CardStackFocus`), which the column of comments on text stacks its threads by (INFRA-5706 R2):
 * the threads stand in the order of their words, none covers another, each level with its words or
 * 8 px or more under the thread above, and the thread in focus level with its words, the threads
 * above it moved up and those under it down. A region that provides no focus - the anchored
 * comments, the decisions beside their blocks - stacks as it did before the rule.
 *
 * `focusTops` is read as a function of numbers. The hook is read through a region drawn with one
 * box per card: happy-dom lays nothing out, so each box is given the height its card has
 * (`offsetHeight`, which the stack reads) as it is drawn, and each card wants the top it names
 * against the region's origin, which is 0 here.
 */

/** The room the stack leaves between two cards, in pixels. */
const GAP = 8;

/** One card of a drawn region: its key, the top it wants (none where it wants none yet), and its
 *  height. */
interface Card {
  readonly height: number;
  readonly key: string;
  readonly top: number | undefined;
}

/** Four cards in the order of their words, 50 px tall, the second wanting a top above the first's
 *  bottom, the third above the second's, and the fourth far below them all. */
const CARDS: readonly Card[] = [
  { height: 50, key: "first", top: 100 },
  { height: 50, key: "second", top: 120 },
  { height: 50, key: "third", top: 140 },
  { height: 50, key: "fourth", top: 400 },
];

/** The keys, the wanted tops and the heights of `cards`, as `focusTops` takes them. */
function shapeOf(cards: readonly Card[]): {
  readonly heights: ReadonlyMap<string, number>;
  readonly keys: string[];
  readonly tops: ReadonlyMap<string, number | undefined>;
} {
  return {
    heights: new Map(cards.map((card) => [card.key, card.height])),
    keys: cards.map((card) => card.key),
    tops: new Map(cards.map((card) => [card.key, card.top])),
  };
}

/** The tops `focusTops` gives `cards` with `focused` in focus, in the cards' order. */
function topsWith(cards: readonly Card[], focused: string | undefined): number[] {
  const { heights, keys, tops } = shapeOf(cards);
  const placed = focusTops(keys, tops, heights, focused);
  return keys.map((key) => placed.get(key) ?? Number.NaN);
}

/** A card's key in the drawn region. */
const keyOfCard = (card: Card): string => card.key;

/** One region stacked by the hook, each card a box of its height at the top the stack gives it. */
function Region({ cards }: { readonly cards: readonly Card[] }): ReactNode {
  const wantedTop = useCallback(
    (card: Card, origin: number) => (card.top === undefined ? undefined : card.top - origin),
    []
  );
  const { cardRef, height, region, topOf } = useCardStack(cards, keyOfCard, wantedTop);
  return createElement(
    "section",
    { ref: region, style: { minHeight: height } },
    cards.map((card) =>
      createElement("div", {
        "data-card": card.key,
        key: card.key,
        ref: (element: HTMLDivElement | null) => {
          if (element !== null) {
            Object.defineProperty(element, "offsetHeight", {
              configurable: true,
              value: card.height,
            });
          }
          cardRef(card.key)(element);
        },
        style: { position: "absolute", top: topOf(card.key) },
      })
    )
  );
}

/** `Region` under a `CardStackFocus` of `focused`, or under no provider where `focused` is the
 *  string "none". */
function regionUnder(cards: readonly Card[], focused: string | undefined | "none"): ReactNode {
  const region = createElement(Region, { cards });
  return focused === "none"
    ? region
    : createElement(CardStackFocus.Provider, { value: focused }, region);
}

/** The top of each box the region `container` draws, in the order of the cards. */
function drawnTops(container: HTMLElement): number[] {
  return [...container.querySelectorAll<HTMLElement>("[data-card]")].map((box) =>
    Number.parseFloat(box.style.top)
  );
}

/** The tops of four cards stacked with no focus: each at the larger of its wanted top and 8 px
 *  under the card before it. */
const UNFOCUSED = [100, 158, 216, 400];

/** Three cards in the order of their words whose second wants a top far above the first's: a
 *  thread placed by its row's first line after one placed by its words lower down that row. */
const OUT_OF_PLACE: readonly Card[] = [
  { height: 40, key: "x", top: 500 },
  { height: 40, key: "y", top: 20 },
  { height: 40, key: "z", top: 900 },
];

test("threads stand in the order of their words", () => {
  // The second thread wants a top above the first's, yet stands after it, with no focus and with
  // each thread in focus.
  const view = render(regionUnder(OUT_OF_PLACE, "none"));
  try {
    expect(
      drawnTops(view.container),
      "the drawn region does not keep the threads in the order of their words"
    ).toEqual([500, 548, 900]);
    for (const cards of [CARDS, OUT_OF_PLACE]) {
      for (const focused of [undefined, ...cards.map((card) => card.key)]) {
        const tops = topsWith(cards, focused);
        expect(
          tops,
          `with ${focused ?? "no thread"} in focus, the threads leave the order of their words`
        ).toEqual([...tops].sort((left, right) => left - right));
      }
    }
  } finally {
    view.unmount();
  }
});

test("no thread covers another, 8 px apart", () => {
  // Stacks of each shape: overlapping words, words far apart, a card that wants no top yet, and
  // cards of unequal heights.
  const stacks: readonly (readonly Card[])[] = [
    CARDS,
    [
      { height: 30, key: "a", top: 0 },
      { height: 120, key: "b", top: 10 },
      { height: 30, key: "c", top: undefined },
      { height: 60, key: "d", top: 140 },
    ],
    OUT_OF_PLACE,
  ];
  for (const cards of stacks) {
    for (const focused of [undefined, ...cards.map((card) => card.key)]) {
      const tops = topsWith(cards, focused);
      cards.forEach((card, at) => {
        if (at === 0) return;
        const above = cards[at - 1] as Card;
        expect(
          tops[at] ?? Number.NaN,
          `with ${focused ?? "no thread"} in focus, ${card.key} is not 8 px or more under ${above.key}`
        ).toBeGreaterThanOrEqual((tops[at - 1] ?? Number.NaN) + above.height + GAP);
      });
    }
  }
  // With no focus, each stands level with its words, or 8 px under the thread above where its
  // words are higher up than that.
  expect(topsWith(CARDS, undefined), "the stack leaves more than 8 px, or less").toEqual(UNFOCUSED);
});

test("a stack with no focus gives the tops it gave before", () => {
  // A region under no provider, as the anchored comments and the decisions are drawn, one under a
  // provider whose focus is none, and one whose focus names no card of it: each places every card
  // as the stack placed it before the focus rule.
  for (const [said, focused] of [
    ["no provider", "none"],
    ["a provider with no focus", undefined],
    ["a focus on no card of the region", "elsewhere"],
  ] as const) {
    const view = render(regionUnder(CARDS, focused));
    try {
      expect(drawnTops(view.container), `under ${said}, the region moved a card`).toEqual(
        UNFOCUSED
      );
    } finally {
      view.unmount();
    }
  }
  expect(topsWith(CARDS, undefined), "the rule with no focus moved a card").toEqual(UNFOCUSED);
});

test("the thread in focus stands level with its words", () => {
  // The third thread's words are at 140, where the stack pushed it down to 216; the focus moving
  // to it, with no other change, stands it at 140.
  const view = render(regionUnder(CARDS, undefined));
  try {
    expect(drawnTops(view.container)[2], "the stack did not push the third thread down").toBe(216);
    view.rerender(regionUnder(CARDS, "third"));
    expect(drawnTops(view.container)[2], "the thread in focus is not level with its words").toBe(
      140
    );
    view.rerender(regionUnder(CARDS, "second"));
    expect(
      drawnTops(view.container)[1],
      "the thread the focus moved to is not level with its words"
    ).toBe(120);
  } finally {
    view.unmount();
  }
});

test("threads above the focus move up and those under it move down", () => {
  // The second thread in focus at its words' 120: the first, whose words are at 100, moves up to
  // end 8 px above it (120 - 8 - 50); the third stands 8 px under it rather than at its words'
  // 140, and the fourth, far below, at its words.
  const expected = [62, 120, 178, 400];
  expect(topsWith(CARDS, "second"), "the rule's tops with the second thread in focus").toEqual(
    expected
  );
  // The third in focus: both threads above it move up as far as they must, each 8 px above the
  // thread under it.
  expect(topsWith(CARDS, "third"), "the rule's tops with the third thread in focus").toEqual([
    24, 82, 140, 400,
  ]);
  const view = render(regionUnder(CARDS, "second"));
  try {
    const drawn = drawnTops(view.container);
    expect(drawn, "the drawn region's tops with the second thread in focus").toEqual(expected);
    expect(drawn[0] ?? Number.NaN, "the thread above the focus did not move up").toBeLessThan(
      CARDS[0]?.top ?? Number.NaN
    );
    expect(
      drawn[2] ?? Number.NaN,
      "the thread under the focus did not move down from its words"
    ).toBeGreaterThan(CARDS[2]?.top ?? Number.NaN);
  } finally {
    view.unmount();
  }
});

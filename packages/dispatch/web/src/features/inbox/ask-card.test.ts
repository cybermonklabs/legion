import { afterEach, expect, test } from "bun:test";

import { outsideAskCard } from "./ask-card";

/** What each case drew on the page, taken off again after it. */
const drawn: HTMLElement[] = [];

afterEach(() => {
  for (const box of drawn.splice(0)) box.remove();
});

/** Draws `html` on the page and puts the focus on the one button it holds. */
function focusButtonIn(html: string): void {
  const box = document.createElement("div");
  box.innerHTML = html;
  document.body.append(box);
  drawn.push(box);
  const button = box.querySelector<HTMLButtonElement>("button");
  if (button === null) throw new Error(`the case drew no button: ${html}`);
  button.focus();
  expect(document.activeElement).toBe(button);
}

const ANSWER = '<button type="button">Answer</button>';

/** `AskBody`'s root as it draws an ask to answer (`AskBody.tsx`), around `inner`. */
function openBody(inner: string): string {
  return `<div data-ask-answering="" data-ask-body="">${inner}</div>`;
}

/** A settled ask's record as `AskCompletionCard` draws it, around `inner`: its article, named by
 *  its test id, with the ask body's root inside it, which carries no answering mark. */
function settledRecord(inner: string): string {
  return `<article data-testid="ask-ask-1"><div data-ask-body="">${inner}</div></article>`;
}

test("a key inside an ask card is inside an ask", () => {
  focusButtonIn(`<article data-ask-card="ask-1">${ANSWER}</article>`);
  expect(outsideAskCard()).toBe(false);
});

// Dashboard draws an ask body with no card around it, so the body's own mark holds the page's
// single keys off its controls.
test("a key inside an ask body that draws an ask to answer, with no card around it, is inside an ask", () => {
  focusButtonIn(openBody(ANSWER));
  expect(outsideAskCard()).toBe(false);
});

// A settled ask's record stands outside every card (`AskCard`'s settled branch,
// `doc/AskBlockCard.tsx`) and carries the ask body's mark without the answering one, so the
// page's keys work on its Change answer and Show earlier answers, as they do on `main`.
test("a key on a settled ask's record, outside every ask card, is the page's", () => {
  focusButtonIn(settledRecord('<button type="button">Change answer</button>'));
  expect(outsideAskCard()).toBe(true);
});

test("a key outside every ask card and ask body is the page's", () => {
  focusButtonIn(`${openBody("")}<section>${ANSWER}</section>`);
  expect(outsideAskCard()).toBe(true);
});

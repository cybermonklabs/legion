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

test("a key inside an ask card is inside an ask", () => {
  focusButtonIn(`<article data-ask-card="ask-1">${ANSWER}</article>`);
  expect(outsideAskCard()).toBe(false);
});

// Dashboard draws an ask body with no card around it, so the body's own mark holds the page's
// single keys off its controls.
test("a key inside an ask body is inside an ask", () => {
  focusButtonIn(`<div data-ask-body="">${ANSWER}</div>`);
  expect(outsideAskCard()).toBe(false);
});

test("a key outside every ask card and ask body is the page's", () => {
  focusButtonIn(`<div data-ask-body=""></div><section>${ANSWER}</section>`);
  expect(outsideAskCard()).toBe(true);
});

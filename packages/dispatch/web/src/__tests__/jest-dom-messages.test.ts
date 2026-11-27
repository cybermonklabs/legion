import { expect, test } from "bun:test";

// These rows hold `jest-dom-matchers.ts` in place, which says why the preload needs it: without
// it, a failing `toHaveAttribute` under bun:test reports only
// `Expected this to be instanceof ExpectMatcherUtils` - not the attribute, not the value wanted,
// not the value found - because jest-dom hands the methods of the `utils` bun:test gives
// `expect.extend` on as plain functions, which unbinds them from the object they read; and a
// failing `toHaveStyle` or `toHaveFormValues` throws `diffFn is not a function` or
// `this.utils.diff is not a function`, because that `utils` carries no `diff` at all.
//
// Each row reads the message a failing matcher throws, so it needs an assertion that fails:
// `failureOf` runs it and hands back the text, and refuses an assertion that passed, which would
// otherwise leave the row green while proving nothing.

function failureOf(assertion: () => void): string {
  try {
    assertion();
  } catch (error) {
    return (error as Error).message;
  }
  throw new Error("the assertion passed, so there is no failure message to read");
}

test("a failed toHaveAttribute names the attribute and both values", () => {
  const field = document.createElement("input");
  field.setAttribute("aria-activedescendant", "option-actual");

  const message = failureOf(() =>
    expect(field).toHaveAttribute("aria-activedescendant", "option-expected")
  );

  expect(message).toContain("aria-activedescendant");
  expect(message).toContain("option-expected");
  expect(message).toContain("option-actual");
});

test("a failed toHaveStyle names the style asked for and the style found", () => {
  const column = document.createElement("div");
  column.style.width = "120px";
  document.body.append(column);
  try {
    const message = failureOf(() => expect(column).toHaveStyle({ width: "240px" }));

    expect(message).toContain("width: 240px");
    expect(message).toContain("width: 120px");
  } finally {
    column.remove();
  }
});

test("a failed toHaveFormValues names the values asked for and the values found", () => {
  const form = document.createElement("form");
  const field = document.createElement("input");
  field.name = "title";
  field.value = "the title found";
  form.append(field);

  const message = failureOf(() => expect(form).toHaveFormValues({ title: "the title asked for" }));

  expect(message).toContain("the title asked for");
  expect(message).toContain("the title found");
});

// The suite writes a part of an attribute as `toHaveAttribute(name, expect.stringContaining(…))`
// or `expect.stringMatching(…)`, where it once wrote `expect(el.getAttribute(name)).toContain(…)`
// or `.toMatch(…)`. That holds only while jest-dom's comparison, the `equals` of the context
// `jest-dom-matchers.ts` builds, honours an asymmetric matcher: this row passes on a part the
// attribute holds, and fails, naming what it asked for and what it found, on a part it does not
// hold and on an attribute that is absent, as the hand-written read failed on both.
test("toHaveAttribute takes an asymmetric matcher, and fails as the hand-written read did", () => {
  const chip = document.createElement("span");
  chip.setAttribute("title", "It has not ended, and may yet answer.");

  expect(chip).toHaveAttribute("title", expect.stringContaining("not ended"));
  expect(chip).toHaveAttribute("title", expect.stringMatching(/^It has not ended/));

  const missed = failureOf(() =>
    expect(chip).toHaveAttribute("title", expect.stringContaining("has ended"))
  );
  expect(missed).toContain("has ended");
  expect(missed).toContain("It has not ended, and may yet answer.");

  const absent = failureOf(() =>
    expect(document.createElement("span")).toHaveAttribute(
      "title",
      expect.stringMatching(/not ended/)
    )
  );
  expect(absent).toContain("not ended");
});

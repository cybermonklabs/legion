/**
 * jest-dom's matchers, able to report their own failures under `bun:test`.
 *
 * `expect.extend` hands a matcher a context whose `utils` is a class instance: its
 * `printReceived`, `printExpected`, `matcherHint` and `stringify` live on that instance's
 * prototype and read its internals. jest-dom hands some of them on as plain functions —
 * `printAttribute(this.utils.stringify, …)` — which unbinds them, so the first one it calls that
 * way while building a failure message throws `Expected this to be instanceof ExpectMatcherUtils`
 * and that string is the whole of what the reader is told: not the attribute, not the value
 * wanted, not the value found. A passing assertion never builds a message, so the suite stays
 * green and only a failing run shows it.
 *
 * So each matcher is called with a context of plain bound functions instead. Bun's context also
 * carries no `diff` at all, which `toHaveStyle` and `toHaveFormValues` build their message with,
 * so they threw `diffFn is not a function` and `this.utils.diff is not a function` when they
 * failed; the context gets one here, written from its own `printExpected` and `printReceived`.
 * Of the helpers jest-dom 7.0.1 reads off the context, `diff` was the one Bun's lacks that it
 * calls bare: it also reaches for `printWithType`, which Bun's lacks too, inside a `try` that
 * leaves that line out of the message. Nothing of the matcher's own behaviour changes: the same
 * matchers decide the same pass or fail, and the wrapper only replaces the helpers they build
 * their message with.
 *
 * `web/src/__tests__/jest-dom-messages.test.ts` holds this in place by reading the message a
 * failing `toHaveAttribute`, `toHaveStyle` and `toHaveFormValues` each throw, and asserting that
 * it names the value asked for and the value found. Those three are what its rows prove; any other
 * matcher builds its message from the same helpers, but no row reads its failure.
 */

import * as jestDomMatchers from "@testing-library/jest-dom/matchers";

/** What `expect.extend` gives a matcher as `this`; only the parts this file touches. */
interface MatcherContext {
  readonly utils: object;
}

/** What a matcher answers: whether it passed, and the message it builds if it did not. */
interface MatcherOutcome {
  readonly pass: boolean;
  readonly message: () => string;
}

/** A matcher as `expect.extend` takes one: `unknown` arguments, so the wrapped set is assignable
 *  to Bun's `CustomMatcher`, whose own parameters are contravariant. */
type Matcher = (
  this: MatcherContext,
  ...args: unknown[]
) => MatcherOutcome | Promise<MatcherOutcome>;

/**
 * Every property `source` answers, its own and its prototypes', with each function bound to
 * `receiver` so that it keeps working once it is read off the object. `constructor` is left out:
 * it is of no use to a matcher and binding it would make a class look like a plain function.
 */
function boundProperties(source: object, receiver: object): Record<string, unknown> {
  const bound: Record<string, unknown> = {};
  for (let level: object | null = source; level !== null; level = Object.getPrototypeOf(level)) {
    for (const name of Object.getOwnPropertyNames(level)) {
      if (name === "constructor" || name in bound) continue;
      const value = (source as Record<string, unknown>)[name];
      bound[name] = typeof value === "function" ? value.bind(receiver) : value;
    }
  }
  return bound;
}

/**
 * The `diff` jest-dom's `toHaveStyle` and `toHaveFormValues` build their failure message with,
 * which Bun's context does not carry: the value asked for and the value found, one under the
 * other, each printed by the context's own `printExpected` and `printReceived`. It marks no
 * line-by-line difference; what the message needs is that it names both values.
 */
function diffFrom(
  utils: Record<string, unknown>
): (expected: unknown, received: unknown) => string {
  return (expected, received) => {
    const { printExpected, printReceived } = utils;
    if (typeof printExpected !== "function" || typeof printReceived !== "function") {
      throw new Error("bun:test's matcher context carries no printExpected or printReceived");
    }
    return `Expected:\n${printExpected(expected)}\n\nReceived:\n${printReceived(received)}`;
  };
}

/**
 * jest-dom's matchers, each wrapped to run against a context it can read its helpers off.
 *
 * `bun:test` heads a failure with the name the wrapper was written under rather than the one it
 * is registered as - neither `Object.defineProperty(fn, "name", …)` nor a computed key moves it,
 * measured - so that first line reads `expect(received).jestDomMatcher()` for all of them. The
 * line under it is jest-dom's own hint, which names the matcher that ran.
 */
export function reportableJestDomMatchers(): Record<string, Matcher> {
  const wrapped: Record<string, Matcher> = {};
  for (const [name, matcher] of Object.entries(jestDomMatchers) as [string, Matcher][]) {
    if (typeof matcher !== "function") continue;
    wrapped[name] = function jestDomMatcher(this: MatcherContext, ...args: unknown[]) {
      const context = boundProperties(this, this);
      const utils = boundProperties(this.utils, this.utils);
      utils.diff ??= diffFrom(utils);
      context.utils = utils;
      return matcher.apply(context as unknown as MatcherContext, args);
    };
  }
  return wrapped;
}

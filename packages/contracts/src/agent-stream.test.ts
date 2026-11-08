import { expect, test } from "bun:test";

import { isShellCommandReport, isTypableCommandName } from "./agent-stream";

// The one rule the session's publisher lists a command by and the page accepts one by: a name a
// person types after the slash, as one token.
test("a command name is one token a person can type", () => {
  expect(isTypableCommandName("compact")).toBe(true);
  expect(isTypableCommandName("skill:dispatch")).toBe(true);
  expect(isTypableCommandName("")).toBe(false);
  expect(isTypableCommandName(" ")).toBe(false);
  expect(isTypableCommandName("two words")).toBe(false);
  expect(isTypableCommandName("tab\there")).toBe(false);
  expect(isTypableCommandName("nul\u0000name")).toBe(false);
  expect(isTypableCommandName("del\u007f")).toBe(false);
  expect(isTypableCommandName("c1\u0085")).toBe(false);
});

// The one rule the session writes a `!command`'s report by and the Agents page reads it back by:
// the person's own line, whole, then a blank line, then what the command printed.
test("a turn's text is the report of the `!` message whose body it carries", () => {
  expect(isShellCommandReport("!printf hi\n\n```\nhi\n```", "!printf hi")).toBe(true);
  expect(isShellCommandReport("!true\n\nThe command printed nothing.", "!true")).toBe(true);
  // The body is no command, so no text of a turn is its report.
  expect(isShellCommandReport("printf hi\n\nhi", "printf hi")).toBe(false);
  // The line itself, with nothing after it, is the person's own message rather than a report.
  expect(isShellCommandReport("!printf hi", "!printf hi")).toBe(false);
  // One line break is not the separator, and a report of another command is not this one's.
  expect(isShellCommandReport("!printf hi\nhi", "!printf hi")).toBe(false);
  expect(isShellCommandReport("!printf bye\n\nbye", "!printf hi")).toBe(false);
});

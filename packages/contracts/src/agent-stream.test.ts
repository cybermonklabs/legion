import { expect, test } from "bun:test";

import { isShellCommandReport, isTypableCommandName, shellCommandLine } from "./agent-stream";

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
// the person's own line, whole and fenced, then a blank line, then what the command printed.
test("a turn's text is the report of the `!` message whose body it carries", () => {
  const report = (line: string, rest: string) => `${shellCommandLine(line)}\n\n${rest}`;
  expect(isShellCommandReport(report("!printf hi", "```\nhi\n```"), "!printf hi")).toBe(true);
  expect(isShellCommandReport(report("!true", "The command printed nothing."), "!true")).toBe(true);
  // The body is no command, so no text of a turn is its report.
  expect(isShellCommandReport(report("printf hi", "hi"), "printf hi")).toBe(false);
  // The line itself, with nothing after it, is the person's own message rather than a report.
  expect(isShellCommandReport("!printf hi", "!printf hi")).toBe(false);
  // One line break is not the separator, and a report of another command is not this one's.
  expect(isShellCommandReport(`${shellCommandLine("!printf hi")}\nhi`, "!printf hi")).toBe(false);
  expect(isShellCommandReport(report("!printf bye", "bye"), "!printf hi")).toBe(false);
  // The body is the person's line as they sent it and as Dispatch stored it, every space around
  // the mark kept. A report opening with the trimmed command is the report of no message here,
  // so the page draws that person's line twice: once as their message, once as the report
  // (dispatch://INFRA-812, round 2).
  expect(isShellCommandReport(report("! ls", "```\none.txt\n```"), "! ls")).toBe(true);
  expect(isShellCommandReport(report("!ls", "```\none.txt\n```"), "! ls")).toBe(false);
  expect(
    isShellCommandReport(report("!  git status", "The command printed nothing."), "!  git status")
  ).toBe(true);
  expect(
    isShellCommandReport(report("!git status", "The command printed nothing."), "!  git status")
  ).toBe(false);
  expect(isShellCommandReport(report("!ls\t", "The command printed nothing."), "!ls\t")).toBe(true);
  expect(isShellCommandReport(report("!ls", "The command printed nothing."), "!ls\t")).toBe(false);
});

// A report opens with the person's line inside a code fence (INFRA-3231): a command line is code,
// and Dispatch's writing check reads none, so `!a; b` is never refused as an English sentence.
test("a report opens with the person's line fenced, and one from before the fence still reads", () => {
  expect(shellCommandLine("!printf hi")).toBe("```\n!printf hi\n```");
  // A line holding a run of backticks gets a longer fence, so the run stays inside it.
  expect(shellCommandLine("!echo ```x```")).toBe("````\n!echo ```x```\n````");
  expect(isShellCommandReport("```\n!printf hi\n```\n\n```\nhi\n```", "!printf hi")).toBe(true);
  expect(isShellCommandReport("```\n! ls\n```\n\nThe command printed nothing.", "! ls")).toBe(true);
  // The fence holds the line as the person sent it, so another line's fence is not this one's.
  expect(isShellCommandReport("```\n!ls\n```\n\nThe command printed nothing.", "! ls")).toBe(false);
  // The fenced line alone, with nothing after it, is no report.
  expect(isShellCommandReport("```\n!printf hi\n```", "!printf hi")).toBe(false);
  // A report from before the fence opens with the line bare, and still reads as the report of
  // that line as sent, every space kept.
  expect(isShellCommandReport("!printf hi\n\n```\nhi\n```", "!printf hi")).toBe(true);
  expect(isShellCommandReport("! ls\n\nThe command printed nothing.", "! ls")).toBe(true);
  expect(isShellCommandReport("!ls\n\nThe command printed nothing.", "! ls")).toBe(false);
  expect(isShellCommandReport("!printf hi\nhi", "!printf hi")).toBe(false);
});

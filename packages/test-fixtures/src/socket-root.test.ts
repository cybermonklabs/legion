import { afterEach, expect, test } from "bun:test";
import { tmpdir } from "node:os";
import { captureEnvironment } from "./environment";
import { socketPathRefusal, socketRoot } from "./socket-root";

const restore: (() => void)[] = [];
afterEach(() => {
  for (const undo of restore.splice(0)) undo();
});

/** Runs `body` with `console.error` collected rather than printed, and returns what it printed. */
function errorsFrom(body: () => void): string[] {
  const collected: string[] = [];
  const printed = console.error;
  console.error = (...parts: unknown[]) => {
    collected.push(parts.map((part) => String(part)).join(" "));
  };
  try {
    body();
  } finally {
    console.error = printed;
  }
  return collected;
}

/** Sets TMPDIR to `value` until the test ends. */
function tmpdirIs(value: string): void {
  restore.push(captureEnvironment(["TMPDIR"]));
  process.env["TMPDIR"] = value;
}

test("a socket that fits under tmpdir() goes there, and nothing is said", () => {
  let root: string | undefined;
  const errors = errorsFrom(() => {
    root = socketRoot("fits", () => undefined);
  });
  expect(root).toBe(tmpdir());
  expect(errors).toEqual([]);
});

test("a socket that does not fit under tmpdir() goes under /tmp, said once per process", () => {
  tmpdirIs("/var/a-temporary-directory-too-long-for-the-socket");
  const refusal = (root: string) => (root === "/tmp" ? undefined : `${root} is too long`);
  const roots: string[] = [];
  const errors = errorsFrom(() => {
    roots.push(socketRoot("the falling-back caller", refusal));
    roots.push(socketRoot("the falling-back caller", refusal));
  });
  expect(roots).toEqual(["/tmp", "/tmp"]);
  expect(errors).toEqual([
    "the falling-back caller: /var/a-temporary-directory-too-long-for-the-socket is too long; falling back to /tmp",
  ]);
});

test("a socket that fits under neither is refused, naming both", () => {
  tmpdirIs("/var/another-temporary-directory");
  expect(() => socketRoot("the refused caller", (root) => `${root} is too long`)).toThrow(
    "the refused caller: no temporary root leaves room for its socket: /var/another-temporary-directory is too long; /tmp is too long"
  );
});

test("socketPathRefusal takes a path of 107 bytes and refuses one of 108", () => {
  const fits = `/${"x".repeat(106)}`;
  expect(socketPathRefusal(fits)).toBeUndefined();
  expect(socketPathRefusal(`${fits}y`)).toBe(
    `${fits}y is 108 bytes, past the 107 a unix socket path holds`
  );
});

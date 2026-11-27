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

test.each([
  ["linux", 107],
  ["darwin", 103],
] as const)(
  "on %s, socketPathRefusal takes a path of %d bytes and refuses one a byte longer",
  (platform, limit) => {
    const fits = `/${"x".repeat(limit - 1)}`;
    expect(socketPathRefusal(fits, platform)).toBeUndefined();
    expect(socketPathRefusal(`${fits}y`, platform)).toBe(
      `${fits}y is ${limit + 1} bytes, past the ${limit} a unix socket path holds on ${platform}`
    );
  }
);

test("a 105-byte path, which Linux takes, is refused on macOS, whose sun_path holds 104 bytes", () => {
  const path = `/${"x".repeat(104)}`;
  expect(socketPathRefusal(path, "linux")).toBeUndefined();
  expect(socketPathRefusal(path, "darwin")).toBe(
    `${path} is 105 bytes, past the 103 a unix socket path holds on darwin`
  );
});

test("a system with no limit of its own takes macOS's, the smaller", () => {
  const path = `/${"x".repeat(103)}`;
  expect(socketPathRefusal(path, "freebsd")).toBe(
    `${path} is 104 bytes, past the 103 a unix socket path holds on freebsd`
  );
});

test("with no platform named, socketPathRefusal measures against this process's", () => {
  const path = `/${"x".repeat(104)}`;
  expect(socketPathRefusal(path)).toBe(socketPathRefusal(path, process.platform));
});

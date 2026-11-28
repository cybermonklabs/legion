import { expect, test } from "bun:test";
import { alive, aliveBy } from "./process-alive";

// The test process answers signal 0, so each read below stands in for what /proc gives once the
// signal has found the process: a stat line, or the failure of a process that ended in between.
const failing = (code: string) =>
  aliveBy(() => {
    throw Object.assign(new Error(`${code}: read /proc stat`), { code });
  });

test("a process that ends between the signal and the stat read is gone", () => {
  // The merge group run that failed on this: ESRCH from the read, thrown out of a row's wait.
  expect(failing("ESRCH")(process.pid)).toBe(false);
  expect(failing("ENOENT")(process.pid)).toBe(false);
});

test("a read that fails for any other reason throws", () => {
  expect(() => failing("EACCES")(process.pid)).toThrow("EACCES");
});

test("a running process is alive, and a zombie is not", () => {
  expect(alive(process.pid)).toBe(true);
  expect(aliveBy(() => `${process.pid} (bun) S 1 1 1\n`)(process.pid)).toBe(true);
  expect(aliveBy(() => `${process.pid} (a) Z) S 1 1 1\n`)(process.pid)).toBe(true);
  expect(aliveBy(() => `${process.pid} (bun) Z 1 1 1\n`)(process.pid)).toBe(false);
  // A name may hold a newline: the state is the field after the last `) `, not after the first
  // line's.
  expect(aliveBy(() => `${process.pid} () S 1\n) Z 1 1 1\n`)(process.pid)).toBe(false);
});

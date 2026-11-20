import { expect, test } from "bun:test";
import { nativesEnvironment, nativesWorkerThreads } from "./natives-environment";

test("a process the caller gives no worker counts gets one worker in each pool, and every other name as given", () => {
  const given = { PATH: "/usr/bin", HOME: "/home/test", EMPTY: undefined };
  expect(nativesEnvironment(given)).toEqual({
    ...given,
    TOKIO_WORKER_THREADS: "1",
    RAYON_NUM_THREADS: "1",
  });
  expect(nativesWorkerThreads).toBe("1");
});

test("a worker count the caller gives is kept, each on its own", () => {
  expect(nativesEnvironment({ TOKIO_WORKER_THREADS: "4" })).toEqual({
    TOKIO_WORKER_THREADS: "4",
    RAYON_NUM_THREADS: "1",
  });
  expect(nativesEnvironment({ RAYON_NUM_THREADS: "4" })).toEqual({
    TOKIO_WORKER_THREADS: "1",
    RAYON_NUM_THREADS: "4",
  });
});

test("the given environment is not changed", () => {
  const given: Record<string, string | undefined> = { PATH: "/usr/bin" };
  nativesEnvironment(given);
  expect(given).toEqual({ PATH: "/usr/bin" });
});

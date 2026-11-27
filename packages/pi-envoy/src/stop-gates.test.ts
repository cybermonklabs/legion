import { expect, test } from "bun:test";
import { gatedLogger } from "./stop-gates";

test("a gated logger writes every line before the stop and none once it began, and leaves the logger's other members alone", () => {
  let stopping = false;
  const written: string[] = [];
  const logger = gatedLogger(() => stopping, {
    name: "host",
    warn: (line: string, fields?: Record<string, unknown>) =>
      written.push(`warn ${line}${fields === undefined ? "" : ` ${JSON.stringify(fields)}`}`),
    info: (line: string) => written.push(`info ${line}`),
  });

  logger.warn("envoy: before the stop", { sessionID: "ses_a" });
  logger.info("envoy: before the stop");
  stopping = true;
  logger.warn("envoy: after the stop");
  logger.info("envoy: after the stop");

  expect(written).toEqual([
    'warn envoy: before the stop {"sessionID":"ses_a"}',
    "info envoy: before the stop",
  ]);
  expect(logger.name).toBe("host");
});

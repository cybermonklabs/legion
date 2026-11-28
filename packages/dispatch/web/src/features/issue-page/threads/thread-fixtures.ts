/**
 * Captured thread records shared by the pure unit tests of Threads (`ThreadRow.test.tsx`,
 * `thread-marks.test.ts`, `thread-row.test.ts`), read from the server's answers captured at the
 * stack base (`fixtures/README.md`). Holds no DOM or test-runner imports.
 */
import type { AskThreadPage, CommentThreadPage, MessageThreadPage } from "@legion/contracts";

import asksThreadBody from "./fixtures/asks-thread.json";
import commentsInBody from "./fixtures/comments-in.json";
import commentsInBeforeBody from "./fixtures/comments-in-before.json";
import messagesInBody from "./fixtures/messages-in.json";
import messagesNoneBody from "./fixtures/messages-none.json";
import { type ThreadRecord, threadRecords } from "./thread-records";

/** Every record of the seeded issue's captured thread pages. */
const RECORDS: readonly ThreadRecord[] = [
  ...((commentsInBody as unknown as CommentThreadPage).threads ?? []),
  ...((commentsInBeforeBody as unknown as CommentThreadPage).threads ?? []),
  ...((messagesInBody as unknown as MessageThreadPage).threads ?? []),
  ...((messagesNoneBody as unknown as MessageThreadPage).threads ?? []),
  ...((asksThreadBody as unknown as AskThreadPage).threads ?? []),
].flatMap(threadRecords);

/** The record whose id begins with `prefix`, as the fixtures' README names them. */
export function fixtureRecord(prefix: string): ThreadRecord {
  const found = RECORDS.find((one) => one.id.startsWith(prefix));
  if (found === undefined) throw new Error(`No page of the fixtures holds the record ${prefix}.`);
  return found;
}

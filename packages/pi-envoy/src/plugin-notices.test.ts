import { expect, test } from "bun:test";
import { noticeProblems } from "@legion/test-fixtures/notice-fields";
import {
  SESSION_ID_CHANGED_NOTICE,
  sessionIdChangedText,
  UNREAD_MESSAGES_NOTICE,
  unreadMessagesText,
} from "./envoy-notices";

// Each notice the Envoy extension builds for its own session, each variant of it rendered twice
// from different values in every field: a sentence the same in every notice of its kind is what
// two renderings would share.
test("every plugin notice states its fields", () => {
  const renderings = [
    {
      declared: SESSION_ID_CHANGED_NOTICE,
      variants: [
        [
          sessionIdChangedText("ses_ew_parent", "ses_ew_fork"),
          sessionIdChangedText("019a4c1e-7b2d", "019a4c2f-0e91"),
        ],
      ],
    },
    {
      declared: UNREAD_MESSAGES_NOTICE,
      variants: [[unreadMessagesText(1), unreadMessagesText(3)]],
    },
  ] as const;
  for (const { declared, variants } of renderings) {
    expect(noticeProblems(declared, variants)).toEqual([]);
  }
});

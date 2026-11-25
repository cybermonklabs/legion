/** The judging rules of `e2e/acceptance/restricted-sweep.sh` for Dispatch's two sockets, each held
 *  alone against the fake Dispatch of `restricted-sweep-fake.ts`: the document socket's room and
 *  its control, and the live socket's event feed - the comment and the two direct messages the
 *  run writes while it reads the feed, and their controls, the member's own control also as their
 *  comment and message on an issue of theirs (SWEEP_MEMBER_ISSUE) - and its agent view of the
 *  project's session. Each row with both sockets served would pass but for what it plants. */

import { afterAll, describe, expect, test } from "bun:test";

import { rowLimit } from "./bounded-subprocess";
import {
  type FeedShape,
  failures,
  issueMode,
  ownIssue,
  ownSession,
  passed,
  served,
  stopFakes,
  sweep,
  tgt,
  verdictOf,
  withoutTools,
} from "./restricted-sweep-fake";

afterAll(stopFakes);

/** Words of the project's that name no row whole, which no line copies: a refused upgrade's body,
 *  or an event's text. */
const words = "words only a member of TGT may read";

describe.skipIf(withoutTools)("the document socket", () => {
  test(
    "a row of the project in the body of the member's refused document upgrade is a FAIL",
    async () => {
      const { code, stdout } = await sweep([], {
        feed: { doc: true, docRefusal: { error: "refused", project: "TGT" } },
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual(["/ws/doc/{room}: the refusal holds TGT, a row of TGT"]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: the member is admitted to their own document, and refused
  // TGT's with a body that names nothing of it.
  test(
    "the member's refused document upgrade whose body names nothing of the project is a refusal",
    async () => {
      const { stdout } = await sweep([], { feed: { doc: true }, rows: ["/api/v1/whoami"] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nok      /ws/doc/{room}: http 404 (control: admitted to their own document)\n"
      );
    },
    rowLimit("short")
  );

  // A refusal that holds a row of the project is a FAIL whatever else the run saw of the socket,
  // its control's probe failing included.
  test(
    "a row of the project in the member's refused document upgrade is a FAIL though the control's probe gave no answer",
    async () => {
      const { code, stdout } = await sweep([], {
        feed: { doc: true, docRefusal: { key: tgt.issue, error: words }, ownDocSilent: true },
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([
        `/ws/doc/{room}: the refusal holds ${tgt.issue}, a row of TGT`,
      ]);
      expect(stdout).not.toContain(words);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: the same failed probe, the refusal naming nothing of TGT, is
  // UNKNOWN, and names the probes' answers by their first words alone.
  test(
    "where the control's probe gives no answer, is UNKNOWN, and copies no refusal's text",
    async () => {
      const { code, stdout } = await sweep([], {
        feed: { doc: true, docRefusal: { error: words }, ownDocSilent: true },
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toMatch(
        /^UNKNOWN \/ws\/doc\/\{room\}: a probe failed \(control 1, member 0\): /m
      );
      expect(stdout).not.toContain(words);
      expect(code).toBe(2);
    },
    rowLimit("short")
  );
});

describe.skipIf(withoutTools)("the live socket's event feed", () => {
  test(
    "a row of the project in the body of the member's refused live upgrade is a FAIL",
    async () => {
      const { code, stdout } = await sweep([], {
        feed: { memberRefusal: { error: "refused", project: "TGT" } },
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([
        "/ws/live: the member's refused upgrade holds TGT, a row of TGT",
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The control of the row above: the same refusal, its body naming nothing of TGT, is a refusal.
  test(
    "the member's refused live upgrade whose body names nothing of the project is a refusal",
    async () => {
      const { stdout } = await sweep([], {
        feed: { live: true, memberRefusal: { error: "refused" } },
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain("\nok      /ws/live: the member's upgrade is refused: http 404\n");
    },
    rowLimit("short")
  );

  // A public runner's log is public: a line about either socket names the server's answer by its
  // status or frame types alone, never the body it wrote.
  test(
    "a passing run copies no refused upgrade's text into its lines",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: {
          doc: true,
          docRefusal: { error: words },
          live: true,
          memberRefusal: { error: words },
        },
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nok      /ws/doc/{room}: http 404 (control: admitted to their own document)\n"
      );
      expect(stdout).toContain("\nok      /ws/live: the member's upgrade is refused: http 404\n");
      expect(stdout).not.toContain(words);
      expect(verdictOf(stdout)).toBe(passed);
      expect(code, stdout).toBe(0);
    },
    rowLimit("short")
  );

  // The member's feed held for its window, both controls holding: the full member's feed is sent
  // the comment and the direct message to the project's session the run writes in the window, and
  // the member's feed the direct message the member writes to their own session, which they read.
  test(
    "where the member's feed is held for its window and every control holds, the live check holds",
    async () => {
      const { code, memberWrites, stdout } = await sweep([], {
        ...served,
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toMatch(
        /^ok {6}\/ws\/live: the member is sent 1 events in 1 s, none of TGT; the full member is sent the comment and the direct message, and the member their own direct message \(controls\)$/m
      );
      expect(stdout).toContain(
        "\nok      GET /api/v1/messages/{id} (the member's own direct message): 200 (control)\n"
      );
      for (const suffix of ["", "/blocks"]) {
        expect(stdout).toContain(
          `\nok      GET /api/v1/messages/{id}${suffix} (the live window's direct message): 404 (can fail)\n`
        );
      }
      expect(verdictOf(stdout)).toBe(passed);
      // The member's one write is the direct message to their own session.
      expect(memberWrites).toEqual([`POST /api/v1/agents/${ownSession}/messages`]);
      expect(code, stdout).toBe(0);
    },
    rowLimit("short")
  );

  test(
    "where the member's live feed closes before its window ends, is UNKNOWN",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: { doc: true, live: true, closes: true },
        rows: ["/api/v1/whoami"],
      });
      expect(stdout).toContain(
        "\nUNKNOWN /ws/live: a subscribed feed ended before the observation window finished\n"
      );
      expect(stdout).not.toContain("PASS");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  // The server ends a subscription whose read failed with an `ended` frame and keeps the socket
  // open: the feed no longer sends anything, and a full member's feed on another socket cannot
  // say it did.
  test(
    "where the server ends the member's subscription and keeps the socket open, is UNKNOWN",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: { doc: true, live: true, ends: true },
        rows: ["/api/v1/whoami"],
      });
      expect(stdout).toContain(
        "\nUNKNOWN /ws/live: the server ended the member's events subscription, its socket left open: closed ended behind\n"
      );
      expect(stdout).not.toMatch(/^ok {6}\/ws\/live:/m);
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  test(
    "where the server refuses the member's subscription on an open socket, is UNKNOWN",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: { doc: true, live: true, refuses: true },
        rows: ["/api/v1/whoami"],
      });
      expect(stdout).toContain(
        "\nUNKNOWN /ws/live: the member's events subscription was refused: "
      );
      expect(stdout).not.toMatch(/^ok {6}\/ws\/live:/m);
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  test(
    "where the full member's live feed misses the comment, is UNKNOWN and quotes no event's text",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: { doc: true, live: true, noise: words },
        rows: ["/api/v1/whoami"],
      });
      expect(stdout).toMatch(/^UNKNOWN \/ws\/live: the control did not hold: /m);
      expect(stdout).not.toContain(words);
      expect(code).toBe(2);
    },
    rowLimit("short")
  );

  // A server that sends a restricted member a person's direct message to a session of the project
  // alone, written while the member's feed is open.
  test(
    "the full member's direct message to the project's session, sent to the member's feed, is a FAIL",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: { doc: true, live: true, dmLeak: true },
        rows: ["/api/v1/whoami"],
      });
      const found = failures(stdout);
      expect(found).toHaveLength(1);
      expect(found[0]).toMatch(
        new RegExp(
          `^/ws/live: the member is sent an event holding ([0-9a-f-]{36}|${tgt.session}), of TGT$`
        )
      );
      expect(code).toBe(1);
    },
    rowLimit("short")
  );

  // The member's own direct message is the control of the member's feed itself: a feed that sends
  // the member nothing, their own message included, proves nothing by its silence.
  test(
    "where the member's feed is not sent their own direct message, is UNKNOWN",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: { doc: true, live: true, ownDmLost: true },
        rows: ["/api/v1/whoami"],
      });
      expect(stdout).toContain(
        `\nUNKNOWN /ws/live: the control did not hold: the member was not sent their own direct message to ${ownSession}\n`
      );
      expect(stdout).not.toMatch(/^ok {6}\/ws\/live:/m);
      expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
      expect(code).toBe(2);
    },
    rowLimit("short")
  );
});

/** A run with both sockets served whose member's own control is their comment and message on
 *  OWN-1, the issue of a project that lists them, in place of a direct message to a session:
 *  `feed` changes the fake's sockets, `env` the run's inputs. */
function issueSweep(feed: FeedShape = {}, env: Readonly<Record<string, string>> = {}) {
  return sweep([], {
    env: { ...served.env, ...issueMode, ...env },
    feed: { ...served.feed, ...feed },
    rows: ["/api/v1/whoami"],
  });
}

describe.skipIf(withoutTools)(
  "the live socket's event feed, the member's control an issue of theirs",
  () => {
    // A person who holds no session (a browser's person alone) writes a comment and a message on an
    // issue of their own project in the window: their feed is sent both, and they read the message
    // back. The run writes no direct message as the member.
    test(
      "where the member's feed is sent their own comment and message, and they read the message, the live check holds",
      async () => {
        const { code, memberWrites, stdout } = await issueSweep();
        expect(failures(stdout)).toEqual([]);
        expect(stdout).toMatch(
          /^ok {6}\/ws\/live: the member is sent 2 events in 1 s, none of TGT; the full member is sent the comment and the direct message, and the member their own comment and message on OWN-1 \(controls\)$/m
        );
        expect(stdout).toContain(
          `\nok      GET /api/v1/messages/{id} (the member's own message on ${ownIssue}): 200 (control)\n`
        );
        expect(memberWrites).toEqual([
          `POST /api/v1/issues/${ownIssue}/comments`,
          `POST /api/v1/issues/${ownIssue}/messages`,
        ]);
        expect(verdictOf(stdout)).toBe(passed);
        expect(code, stdout).toBe(0);
      },
      rowLimit("short")
    );

    // The feed cut: a feed that sends the member neither of their own writes proves nothing by its
    // silence about the project's.
    test(
      "where the member's feed is sent neither their own comment nor their message, is UNKNOWN",
      async () => {
        const { code, stdout } = await issueSweep({ ownIssueLost: ["comment", "message"] });
        expect(stdout).toContain(
          `\nUNKNOWN /ws/live: the control did not hold: the member was not sent their own comment and message on ${ownIssue}\n`
        );
        expect(stdout).not.toMatch(/^ok {6}\/ws\/live:/m);
        expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
        expect(code).toBe(2);
      },
      rowLimit("short")
    );

    // Each of the two is the control: a feed sent the comment and not the message holds no more.
    test(
      "where the member's feed is sent their own comment but not their message, is UNKNOWN, naming the message",
      async () => {
        const { code, stdout } = await issueSweep({ ownIssueLost: ["message"] });
        expect(stdout).toContain(
          `\nUNKNOWN /ws/live: the control did not hold: the member was not sent their own message on ${ownIssue}\n`
        );
        expect(stdout).not.toMatch(/^ok {6}\/ws\/live:/m);
        expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
        expect(code).toBe(2);
      },
      rowLimit("short")
    );

    // The other half: a feed sent the message and not the comment holds no more either.
    test(
      "where the member's feed is sent their own message but not their comment, is UNKNOWN, naming the comment",
      async () => {
        const { code, stdout } = await issueSweep({ ownIssueLost: ["comment"] });
        expect(stdout).toContain(
          `\nUNKNOWN /ws/live: the control did not hold: the member was not sent their own comment on ${ownIssue}\n`
        );
        expect(stdout).not.toMatch(/^ok {6}\/ws\/live:/m);
        expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
        expect(code).toBe(2);
      },
      rowLimit("short")
    );

    // A member who cannot write there: an issue of a project that does not list them, which the
    // server answers as an issue they do not see. The comment's refusal ends the check, and no
    // message is written after it.
    test(
      "where the member cannot write on the issue, a project that does not list them, is UNKNOWN, naming the refusal",
      async () => {
        const { code, memberWrites, stdout } = await issueSweep(
          {},
          { SWEEP_MEMBER_ISSUE: "ELSE-1" }
        );
        expect(stdout).toContain(
          "\nUNKNOWN /ws/live: the member's comment on their own issue ELSE-1 answered 404\n"
        );
        expect(memberWrites).toEqual(["POST /api/v1/issues/ELSE-1/comments"]);
        expect(stdout).not.toMatch(/^ok {6}\/ws\/live:/m);
        expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
        expect(code).toBe(2);
      },
      rowLimit("short")
    );

    // The message read back is the control of the rows that read the project's direct message.
    test(
      "where the member's own message reads otherwise than 200, is UNKNOWN",
      async () => {
        const { code, stdout } = await issueSweep({ ownIssueUnread: true });
        expect(stdout).toContain(
          `\nUNKNOWN GET /api/v1/messages/{id} (the member's own message on ${ownIssue}): the control did not hold: the member is answered 404 for their own message on ${ownIssue}\n`
        );
        expect(verdictOf(stdout)).toStartWith("UNKNOWN: ");
        expect(code).toBe(2);
      },
      rowLimit("short")
    );

    // The member's own writes blind no rule: what the project holds, sent to their feed, still FAILs.
    test(
      "the full member's direct message to the project's session, sent to the member's feed, is a FAIL",
      async () => {
        const { code, stdout } = await issueSweep({ dmLeak: true });
        const found = failures(stdout);
        expect(found).toHaveLength(1);
        expect(found[0]).toMatch(
          new RegExp(
            `^/ws/live: the member is sent an event holding ([0-9a-f-]{36}|${tgt.session}), of TGT$`
          )
        );
        expect(code).toBe(1);
      },
      rowLimit("short")
    );
  }
);

describe.skipIf(withoutTools)(
  "the full member's direct message and agent view, as their token",
  () => {
    // The full member's cookie is the GitHub proxy's alone: with it given, the direct message is
    // still written, and the agent view still watched, as the full member's token.
    test(
      "with the full member's cookie given, the direct message and the agent view are the token's, and the run passes",
      async () => {
        const { code, directBy, stdout } = await sweep([], { ...served, rows: ["/api/v1/whoami"] });
        expect(directBy).toEqual(["token"]);
        expect(stdout).toContain(
          "\nok      /ws/live agent view of {session_id}: no one answers for it (can fail)\n"
        );
        expect(verdictOf(stdout)).toBe(passed);
        expect(code, stdout).toBe(0);
      },
      rowLimit("short")
    );

    // A token without the person scope, as the shared agent token: the server refuses it the direct
    // message (403) and the agent view (an `error` frame), so neither control holds, though the
    // cookie that would have been answered is given.
    test(
      "a full member's token without the person scope leaves the direct message's and the agent view's controls unread: UNKNOWN",
      async () => {
        const { code, stdout } = await sweep([], {
          ...served,
          rows: ["/api/v1/whoami"],
          tokenScope: "none",
        });
        expect(failures(stdout)).toEqual([]);
        expect(stdout).toMatch(
          /^UNKNOWN \/ws\/live: the full member's direct message to ses-tgt-worker answered 403: /m
        );
        expect(stdout).toContain(
          "\nUNKNOWN /ws/live agent view of {session_id}: the full member's view was answered error, closed refused, so its control was not read\n"
        );
        expect(stdout).not.toContain("PASS");
        expect(code).toBe(2);
      },
      rowLimit("short")
    );

    // A restricted member's token in the full member's place: the server answers it the session as
    // one nobody holds (404 SESSION_NOT_FOUND) and the agent view as no one answers, so the direct
    // message's control does not hold and the agent view cannot fail.
    test(
      "a restricted member's token in the full member's place: the direct message is answered 404 and the agent view is blind, UNKNOWN",
      async () => {
        const { code, stdout } = await sweep([], {
          ...served,
          rows: ["/api/v1/whoami"],
          tokenScope: "restricted",
        });
        expect(failures(stdout)).toEqual([]);
        expect(stdout).toMatch(
          /^UNKNOWN \/ws\/live: the full member's direct message to ses-tgt-worker answered 404: /m
        );
        expect(stdout).toContain(
          "\nok      /ws/live agent view of {session_id}: no one answers for it (blind)\n"
        );
        expect(stdout).not.toContain("PASS");
        expect(code).toBe(2);
      },
      rowLimit("short")
    );
  }
);

describe.skipIf(withoutTools)("the live socket's agent view", () => {
  // The member subscribes to the conversation of the project's session: they must be answered as
  // for a session no one answers for, where the full member is answered that it responds, and its
  // replay.
  test(
    "the member answered that no one answers for the project's session is no FAIL, and can fail",
    async () => {
      const { stdout } = await sweep([], { ...served, rows: ["/api/v1/whoami"] });
      expect(failures(stdout)).toEqual([]);
      expect(stdout).toContain(
        "\nok      /ws/live agent view of {session_id}: no one answers for it (can fail)\n"
      );
    },
    rowLimit("short")
  );

  test(
    "the member sent the project's session's conversation is a FAIL",
    async () => {
      const { code, stdout } = await sweep([], {
        ...served,
        feed: { doc: true, live: true, agentLeak: true },
        rows: ["/api/v1/whoami"],
      });
      expect(failures(stdout)).toEqual([
        "/ws/live agent view of {session_id}: the member is sent more of the session than that no one answers for it: responder, replay",
      ]);
      expect(code).toBe(1);
    },
    rowLimit("short")
  );
});

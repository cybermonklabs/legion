/** The judging rules of `e2e/acceptance/restricted-sweep.sh` for Dispatch's two sockets, each held
 *  alone against the fake Dispatch of `restricted-sweep-fake.ts`: the document socket's room and
 *  its control, and the live socket's event feed - the comment and the two direct messages the
 *  run writes while it reads the feed, and their controls - and its agent view of the project's
 *  session. Each row with both sockets served would pass but for what it plants. */

import { afterAll, describe, expect, test } from "bun:test";

import { rowLimit } from "./bounded-subprocess";
import {
  failures,
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
      const { code, stdout } = await sweep([], { ...served, rows: ["/api/v1/whoami"] });
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

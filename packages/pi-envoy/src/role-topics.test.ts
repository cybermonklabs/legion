import { describe, expect, test } from "bun:test";
import { createRoleTopicFollows, readRoleTopics } from "./role-topics";

const ALERTS = "notifications.slack.*.C0ALERTS.message";

describe("readRoleTopics", () => {
  test("unset or blank, no role follows a topic and nothing is refused", () => {
    for (const value of [undefined, "", "  \n"]) {
      const read = readRoleTopics(value);
      expect(read.error).toBeUndefined();
      expect(read.topicsFor("ops-owner")).toEqual([]);
    }
  });

  test("names each role's topics, read as the listener reads a role's name", () => {
    const read = readRoleTopics(
      JSON.stringify({
        "ops-owner": [ALERTS, ALERTS],
        " legion-acme-controller\t": [ALERTS, "notifications.github.acme.app.>"],
      })
    );
    expect(read.error).toBeUndefined();
    // A topic named twice is followed once.
    expect(read.topicsFor("ops-owner")).toEqual([ALERTS]);
    expect(read.topicsFor("legion-acme-controller")).toEqual([
      ALERTS,
      "notifications.github.acme.app.>",
    ]);
    expect(read.topicsFor(" legion-acme-controller ")).toEqual(
      read.topicsFor("legion-acme-controller")
    );
    expect(read.topicsFor("reviewer")).toEqual([]);
  });

  for (const [label, value, why] of [
    ["text that is not JSON", `ops-owner=${ALERTS}`, "it is not JSON"],
    [
      "a list",
      JSON.stringify([ALERTS]),
      "it is not a JSON object of role names to lists of topics",
    ],
    ["an empty role name", JSON.stringify({ " ": [ALERTS] }), '" " names no role'],
    [
      "one role named twice",
      `{"ops-owner": [${JSON.stringify(ALERTS)}], " ops-owner": []}`,
      "role ops-owner is named twice",
    ],
    [
      // JSON.parse keeps the last of two keys of one spelling, so the refusal reads the text.
      "the same role name twice, letter for letter",
      `{"ops-owner": [${JSON.stringify(ALERTS)}], "ops-owner": []}`,
      "role ops-owner is named twice",
    ],
    [
      "a `>` before the last token, which a consumer filter refuses",
      JSON.stringify({ "ops-owner": ["notifications.slack.>.C0ALERTS.message"] }),
      'role ops-owner: "notifications.slack.>.C0ALERTS.message" is not a NATS subject',
    ],
    [
      "a wildcard inside a token",
      JSON.stringify({ "ops-owner": ["notifications.slack.*x.C0ALERTS.message"] }),
      'role ops-owner: "notifications.slack.*x.C0ALERTS.message" is not a NATS subject',
    ],
    [
      "a role with no list",
      JSON.stringify({ "ops-owner": ALERTS }),
      "role ops-owner names no list of topics",
    ],
    [
      "a topic no stream stores",
      JSON.stringify({ "ops-owner": ["notifications.role.ops-owner"] }),
      'role ops-owner: the notification stream stores nothing on "notifications.role.ops-owner", so it would deliver nothing',
    ],
    [
      "another session's agent subject",
      JSON.stringify({ "ops-owner": ["notifications.agent.>"] }),
      'role ops-owner: "notifications.agent.>" reaches a subject private to one session, which the listener stores for nobody else',
    ],
    [
      "a subject with an empty token",
      JSON.stringify({ "ops-owner": ["notifications.slack..C0ALERTS.message"] }),
      'role ops-owner: "notifications.slack..C0ALERTS.message" is not a NATS subject',
    ],
  ] as const) {
    test(`refuses the whole variable for ${label}`, () => {
      const read = readRoleTopics(value);
      expect(read.error).toBe(
        `ENVOY_ROLE_TOPICS is refused, so no role follows a topic it names: ${why}`
      );
      expect(read.topicsFor("ops-owner")).toEqual([]);
    });
  }

  test("a refusal anywhere takes no role's topics, not even a valid role's", () => {
    const read = readRoleTopics(
      JSON.stringify({ "ops-owner": [ALERTS], reviewer: ["notifications.agent.ses_x"] })
    );
    expect(read.error).toContain("role reviewer");
    expect(read.topicsFor("ops-owner")).toEqual([]);
  });

  test("says which subjects it names, for the resume rule that leaves them to the role's claim", () => {
    const read = readRoleTopics(JSON.stringify({ "ops-owner": ["notifications.team.alerts.>"] }));
    expect(read.error).toBeUndefined();
    // A `>` topic names its own base and itself, as a subscription expands it.
    expect(read.names("notifications.team.alerts")).toBe(true);
    expect(read.names("notifications.team.alerts.>")).toBe(true);
    expect(read.names("notifications.team.other")).toBe(false);
  });
});

// What the session follows of a role's topics: only what the role's own opens added, so a topic
// the session already followed itself stays its own and the role's end does not take it away.
describe("createRoleTopicFollows", () => {
  const BUILDS = "notifications.slack.*.C0BUILDS.message";
  const roleTopics = readRoleTopics(
    JSON.stringify({ "ops-owner": [ALERTS, BUILDS], reviewer: [BUILDS] })
  );

  /** The follows, over a session that already follows `already`. The extension unfollows what a
   *  role's end closes (`closeSubjects`) and what an edge listener refused (`drop`), so this
   *  stand-in does too. */
  const follows = (...already: string[]) => {
    const followed = new Set(already);
    const open = createRoleTopicFollows({
      roleTopics,
      follow: (topic) => {
        if (followed.has(topic)) return [];
        followed.add(topic);
        return [topic];
      },
    });
    return {
      ...open,
      close: () => {
        const closed = open.close();
        for (const subject of closed) followed.delete(subject);
        return closed;
      },
      refuse: (subjects: readonly string[]) => {
        for (const subject of subjects) followed.delete(subject);
        open.refuse(subjects);
      },
    };
  };

  test("keeps only the subjects its own open followed", () => {
    const open = follows(ALERTS);
    open.open("ops-owner");

    // ALERTS was the session's own before the claim, so it is not the role's and the role's end
    // leaves it followed; BUILDS is the role's.
    expect(open.has(ALERTS)).toBe(false);
    expect(open.subjects()).toEqual([BUILDS]);
    expect(open.close()).toEqual([BUILDS]);
    expect(open.subjects()).toEqual([]);
  });

  test("owes a registration until one carries every subject it followed", () => {
    const open = follows();
    expect(open.owed()).toBe(false);
    open.open("ops-owner");
    expect(open.owed()).toBe(true);
    open.registered([ALERTS]);
    expect(open.owed()).toBe(true);
    open.registered([ALERTS, BUILDS]);
    expect(open.owed()).toBe(false);
    // A role the variable names no topic for follows nothing and owes nothing.
    open.close();
    open.open("architect");
    expect(open.subjects()).toEqual([]);
    expect(open.owed()).toBe(false);
  });

  test("a refused subject leaves the role's and is not followed again until the role ends", () => {
    const open = follows();
    open.open("ops-owner");
    open.refuse([ALERTS]);
    expect(open.subjects()).toEqual([BUILDS]);

    // A later open of the same role does not ask for it again, so no heartbeat re-sends it.
    open.open("ops-owner");
    expect(open.subjects()).toEqual([BUILDS]);
    // The role's end forgets the refusal: the next hold asks for it once more.
    open.close();
    open.open("ops-owner");
    expect(open.subjects()).toEqual([ALERTS, BUILDS]);
  });
});

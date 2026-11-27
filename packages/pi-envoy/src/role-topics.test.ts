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

  // Each refusal names the role or the topic that caused it; the sentence around it is the
  // plugin's wording, which no program parses, so only the documented prefix is held, in one row.
  const LONG_TOPIC = `notifications.slack.*.${"C".repeat(4_032)}.message`;
  for (const [label, value, names] of [
    ["text that is not JSON", `ops-owner=${ALERTS}`, "not JSON"],
    ["a list", JSON.stringify([ALERTS]), "not a JSON object"],
    ["an empty role name", JSON.stringify({ " ": [ALERTS] }), '" "'],
    [
      "one role named twice",
      `{"ops-owner": [${JSON.stringify(ALERTS)}], " ops-owner": []}`,
      "role ops-owner",
    ],
    [
      // JSON.parse keeps the last of two keys of one spelling, so the refusal reads the text.
      "the same role name twice, letter for letter",
      `{"ops-owner": [${JSON.stringify(ALERTS)}], "ops-owner": []}`,
      "role ops-owner",
    ],
    [
      "a `>` before the last token, which the listener's consumer filter refuses",
      JSON.stringify({ "ops-owner": ["notifications.slack.>.C0ALERTS.message"] }),
      "notifications.slack.>.C0ALERTS.message",
    ],
    [
      "a wildcard inside a token, which the rule takes as no deployment's meaning",
      JSON.stringify({ "ops-owner": ["notifications.slack.*x.C0ALERTS.message"] }),
      "notifications.slack.*x.C0ALERTS.message",
    ],
    [
      "a subject past the listener's 4,032 bytes",
      JSON.stringify({ "ops-owner": [LONG_TOPIC] }),
      "role ops-owner",
    ],
    ["a role with no list", JSON.stringify({ "ops-owner": ALERTS }), "role ops-owner"],
    [
      "a topic no stream stores",
      JSON.stringify({ "ops-owner": ["notifications.role.ops-owner"] }),
      "notifications.role.ops-owner",
    ],
    [
      "another session's agent subject",
      JSON.stringify({ "ops-owner": ["notifications.agent.>"] }),
      "notifications.agent.>",
    ],
    [
      "a subject with an empty token",
      JSON.stringify({ "ops-owner": ["notifications.slack..C0ALERTS.message"] }),
      "notifications.slack..C0ALERTS.message",
    ],
  ] as const) {
    test(`refuses the whole variable for ${label}`, () => {
      const read = readRoleTopics(value);
      expect(read.error).toContain(names);
      expect(read.topicsFor("ops-owner")).toEqual([]);
      expect(read.names(ALERTS)).toBe(false);
    });
  }

  test("a refusal opens with the prefix the README documents", () => {
    expect(readRoleTopics("[]").error).toStartWith(
      "ENVOY_ROLE_TOPICS is refused, so no role follows a topic it names: "
    );
  });

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

  test("a subject the session already followed is its own as well as the role's", () => {
    const open = follows(ALERTS);
    open.open("ops-owner");

    // The role wants both; only BUILDS is the role's alone, so the role's end closes only it and
    // leaves the session's own ALERTS followed.
    expect(open.subjects()).toEqual([ALERTS, BUILDS]);
    expect(open.has(ALERTS)).toBe(true);
    expect(open.roleOnly(ALERTS)).toBe(false);
    expect(open.roleOnly(BUILDS)).toBe(true);
    expect(open.close()).toEqual([BUILDS]);
    expect(open.subjects()).toEqual([]);
  });

  test("an unsubscribe hands its own subject to the role, and a subscribe takes the role's", () => {
    const open = follows(ALERTS);
    open.open("ops-owner");

    // The session drops its own claim on ALERTS while the role holds it: it stays followed, now
    // for the role alone, so the role's end closes it.
    expect(open.releaseOwn(ALERTS)).toBe(true);
    expect(open.releaseOwn(ALERTS)).toBe(false);
    expect(open.roleOnly(ALERTS)).toBe(true);
    // A subject the role does not want is not the role's to keep.
    expect(open.releaseOwn("notifications.slack.*.C0OTHER.message")).toBe(false);

    // The session subscribes to BUILDS, which the role wanted alone: it is its own too now, so
    // the role's end leaves it.
    expect(open.claimOwn(BUILDS)).toBe(true);
    expect(open.claimOwn(BUILDS)).toBe(false);
    expect(open.close()).toEqual([ALERTS]);
  });

  test("the role's end answers only a `>` topic's own subject when the session follows its base", () => {
    const BASE = "notifications.github.acme.app";
    const open = createRoleTopicFollows({
      roleTopics: readRoleTopics(JSON.stringify({ "ops-owner": [`${BASE}.>`] })),
      // The session follows the base itself, so the role's follow adds only `.>`.
      follow: (topic) => (topic === `${BASE}.>` ? [`${BASE}.>`] : []),
    });
    open.open("ops-owner");
    expect(open.subjects()).toEqual([BASE, `${BASE}.>`]);
    expect(open.roleOnly(BASE)).toBe(false);
    // The caller deletes each closed subject exactly, so the base stays followed.
    expect(open.close()).toEqual([`${BASE}.>`]);
  });

  test("remembers the role it opened for, until the role's end", () => {
    const open = follows();
    expect(open.role()).toBeUndefined();
    open.open("ops-owner");
    expect(open.role()).toBe("ops-owner");
    open.close();
    expect(open.role()).toBeUndefined();
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

import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import {
  controllerToken,
  legionControllerNoticeSubject,
  NO_PROJECT,
  uncapturedTopicRefusal,
} from "@legion/contracts";
import { deliverSubject } from "../consumer-delivery";
import {
  agentSubjectSession,
  controllerNoticeRole,
  isOwnAgentSubject,
  ownAgentSubject,
  placedRoleLane,
  placedRoleName,
  placeRole,
  reachesPrivateStreamSubject,
  refuseUncapturedTopics,
  registrationTopics,
  rememberedTopics,
  replayFollowedSubjects,
  roleOfLane,
  streamDeliversNothing,
  streamStores,
} from "../stream-topics";

test("the stream stores agent, producer and team topics, wildcards read literally", () => {
  expect(streamStores("notifications.agent.ses_a")).toBe(true);
  expect(streamStores("notifications.dispatch.issue.DSP-7.>")).toBe(true);
  expect(streamStores("notifications.github.acme.repo.issue.3")).toBe(true);
  expect(streamStores("notifications.team.deploy")).toBe(true);
  expect(streamStores("notifications.envoy.exceptions.notifications.agent.ses_a")).toBe(true);
  expect(streamStores("notifications.envoy.exceptions.notifications.team.deploy")).toBe(true);
});

test("an uncaptured topic, bare prefixes and the relay subject are not stored", () => {
  expect(streamStores("notifications.lxspec.probe.all")).toBe(false);
  expect(streamStores("notifications.teamx.deploy")).toBe(false);
  expect(streamStores("notifications.dispatch.")).toBe(false);
  expect(streamStores("notifications.>")).toBe(false);
  expect(streamStores(deliverSubject("ses_a"))).toBe(false);
});

test("a topic nothing would be delivered on is one no stream subject captures, a role lane apart", () => {
  // What the listener leaves out of a registration, and refuses a publish to by name
  // (`bus.ReachesNoSession`).
  expect(streamDeliversNothing("notifications.team")).toBe(true);
  expect(streamDeliversNothing("notifications.github")).toBe(true);
  expect(streamDeliversNothing("notifications.lxspec.probe.all")).toBe(true);
  expect(streamDeliversNothing("notifications.envoy.deliver.ses_a")).toBe(true);
  // A role lane travels over core NATS and its holder registers it.
  expect(streamDeliversNothing("notifications.role.legion-omp-controller")).toBe(false);
  expect(streamDeliversNothing("notifications.team.deploys")).toBe(false);
  expect(streamDeliversNothing("notifications.agent.ses_a")).toBe(false);
  // Only a notifications. topic is the listener's to refuse by name.
  expect(streamDeliversNothing("team.deploys")).toBe(false);
});

test("a host refuses such a topic in the listener's own words", () => {
  // The sentence a host's explicit subscribe refuses with is written once, in @legion/contracts,
  // and the listener's warning for the same topic is generated from it
  // (contracts.UncapturedTopicRefusal in packages/envoy/internal/contracts/generated.go): the two
  // are one wording, so a caller reads the same reason whether its topic was refused here or
  // dropped there. What this pins is the sentence itself, which the generated Go copy is checked
  // against by packages/contracts' own `bun run check:go`.
  expect(uncapturedTopicRefusal("notifications.team", "ses_a")).toBe(
    "notifications.team was not registered: no stream subject captures it, so nothing would be delivered to ses_a on it; follow a notifications.team.<name> topic instead"
  );
  expect(uncapturedTopicRefusal("notifications.lxspec.probe", "ses_b")).toContain(
    "notifications.lxspec.probe"
  );
  expect(uncapturedTopicRefusal("notifications.lxspec.probe", "ses_b")).toContain("ses_b");
});

test("a host's subscribe refuses an uncaptured topic, and no private subject", () => {
  const own = "notifications.agent.ses_a";
  // The listener leaves these out of a registration with no caller to tell; its tool refuses
  // them, since a caller named them.
  expect(() => refuseUncapturedTopics("ses_a", own, ["notifications.team"])).toThrow(
    uncapturedTopicRefusal("notifications.team", "ses_a")
  );
  expect(() => refuseUncapturedTopics("ses_a", own, ["notifications.lxspec.probe"])).toThrow(
    uncapturedTopicRefusal("notifications.lxspec.probe", "ses_a")
  );
  // A private stream subject is judged as one, by the listener and here alike: a host opens one
  // itself, so it stays followed and is left out of the registration without a refusal.
  expect(() => refuseUncapturedTopics("ses_a", own, ["notifications.>"])).not.toThrow();
  expect(() => refuseUncapturedTopics("ses_a", own, ["notifications.*.x"])).not.toThrow();
  expect(() => refuseUncapturedTopics("ses_a", own, ["notifications.agent.*"])).not.toThrow();
  // A stored topic, the session's own subject and a role lane are no one's to refuse.
  expect(() => refuseUncapturedTopics("ses_a", own, [own])).not.toThrow();
  expect(() => refuseUncapturedTopics("ses_a", own, ["notifications.team.deploys"])).not.toThrow();
  expect(() => refuseUncapturedTopics("ses_a", own, ["notifications.role.reviewer"])).not.toThrow();
  expect(() => refuseUncapturedTopics("ses_a", own, ["team.deploys"])).not.toThrow();
});

describe("a controller notice lane", () => {
  test("names the role token its project's controller holds, as contracts mints it", () => {
    expect(controllerNoticeRole(legionControllerNoticeSubject("acme"))).toBe(
      controllerToken("acme")
    );
    expect(controllerNoticeRole("notifications.legion.ACME.controller")).toBe(
      "legion-ACME-controller"
    );
  });

  test("is one literal subject: no wildcard, no empty project, no issue notice", () => {
    expect(controllerNoticeRole("notifications.legion.*.controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.>")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion..controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.acme.LEGION-7")).toBeUndefined();
    expect(controllerNoticeRole("notifications.legion.acme.sub.controller")).toBeUndefined();
    expect(controllerNoticeRole("notifications.agent.ses_a")).toBeUndefined();
  });
});

/**
 * The listener's own cases: `packages/envoy/internal/bus/testdata/private-stream-subjects.json`,
 * which `TestReachesPrivateStreamSubjectBoundsInterests` in packages/envoy/internal/bus reads
 * too. One file, read by both, so a case added for either is run by both on the next test run
 * and neither side can drift from a hand copy.
 */
const SHARED_CASES = resolve(
  import.meta.dir,
  "../../../envoy/internal/bus/testdata/private-stream-subjects.json"
);

interface SharedCase {
  readonly sessionID: string;
  readonly topic: string;
  readonly roles?: readonly string[];
  readonly want: boolean;
}

test("a topic reaching a private stream subject is the listener's rule, case for case", () => {
  const cases: SharedCase[] = JSON.parse(readFileSync(SHARED_CASES, "utf8"));
  // The file held 37 cases when this was written: a read that answers a handful of them, or
  // none, passes while covering nothing.
  expect(cases.length).toBeGreaterThanOrEqual(37);
  for (const { sessionID, topic, roles = [], want } of cases) {
    // The session's own subject as the old form names it and as a host now registers it, the
    // new form over every project: the rule reads the two alike.
    for (const own of [`notifications.agent.${sessionID}`, ownAgentSubject(sessionID)]) {
      expect({
        own,
        topic,
        roles,
        reaches: reachesPrivateStreamSubject(own, topic, roles),
      }).toEqual({ own, topic, roles, reaches: want });
    }
  }
});

describe("a session's own agent subject", () => {
  test("is its messages under every project, and reads back in either form", () => {
    expect(ownAgentSubject("ses_a")).toBe("notifications.agent.*.ses_a");
    for (const subject of [
      "notifications.agent.ses_a",
      "notifications.agent.*.ses_a",
      `notifications.agent.${NO_PROJECT}.ses_a`,
      "notifications.agent.PRIV.ses_a",
    ]) {
      expect([subject, agentSubjectSession(subject), isOwnAgentSubject(subject, "ses_a")]).toEqual([
        subject,
        "ses_a",
        true,
      ]);
    }
    for (const subject of [
      "notifications.agent.ses_b",
      "notifications.agent.PRIV.ses_b",
      // A token in the project's place that is no project names no one session.
      "notifications.agent.priv.ses_a",
      "notifications.agent.x.y.ses_a",
      "notifications.role.ses_a",
      "notifications.agent.",
    ]) {
      expect([subject, isOwnAgentSubject(subject, "ses_a")]).toEqual([subject, false]);
    }
    // A wildcard where the session would be is no session's alone.
    expect(agentSubjectSession("notifications.agent.*")).toBe("*");
    expect(isOwnAgentSubject("notifications.agent.*", "ses_a")).toBe(false);
  });
});

describe("the project a role is claimed under", () => {
  test("a Legion role works for its own project, whatever the token lists", () => {
    for (const projects of [undefined, [], ["PRIV"], ["PRIV", "PUB"]]) {
      expect(placeRole("legion-infra-infra-12-architect", projects)).toEqual({
        project: "INFRA",
        role: "legion-infra-infra-12-architect",
      });
    }
  });

  test("a free-form role works for the one project the token lists, else for none", () => {
    expect(placeRole("reviewer", ["PRIV"])).toEqual({ project: "PRIV", role: "reviewer" });
    for (const projects of [undefined, [], ["PRIV", "PUB"]]) {
      expect([projects, placeRole("reviewer", projects)]).toEqual([
        projects,
        { project: NO_PROJECT, role: "reviewer" },
      ]);
    }
    // Only a project key is a project: the Legion project token is never one.
    expect(placeRole("reviewer", ["infra"])).toEqual({ project: NO_PROJECT, role: "reviewer" });
  });

  test("a role named in the new form keeps the project it names", () => {
    expect(placeRole("PUB.reviewer", ["PRIV"])).toEqual({ project: "PUB", role: "reviewer" });
    expect(placeRole("_.reviewer", ["PRIV"])).toEqual({ project: NO_PROJECT, role: "reviewer" });
  });

  test("a placed role's claim name and lane name its project first", () => {
    const placed = placeRole("reviewer", ["PRIV"]);
    expect(placedRoleName(placed)).toBe("PRIV.reviewer");
    expect(placedRoleLane(placed)).toBe("notifications.role.PRIV.reviewer");
    expect(placedRoleLane(placeRole("legion-infra-infra-12-architect", undefined))).toBe(
      "notifications.role.INFRA.legion-infra-infra-12-architect"
    );
  });

  test("a lane reads back to its role in either form, a claim's bare key included", () => {
    expect(roleOfLane("notifications.role.PRIV.reviewer")).toEqual({
      project: "PRIV",
      role: "reviewer",
    });
    expect(roleOfLane("notifications.role.reviewer")).toEqual({
      project: NO_PROJECT,
      role: "reviewer",
    });
    expect(roleOfLane("notifications.role.legion-infra-infra-12-architect")).toEqual({
      project: "INFRA",
      role: "legion-infra-infra-12-architect",
    });
    expect(roleOfLane("notifications.role.")).toBeUndefined();
    expect(roleOfLane("notifications.agent.ses_a")).toBeUndefined();
  });
});

describe("the subjects a restore follows again", () => {
  const ISSUE = "notifications.dispatch.issue.DSP-1.>";

  test("are the session's own agent subject and its topics, never a role's", () => {
    const followed = replayFollowedSubjects(
      "notifications.agent.ses-1",
      [
        ISSUE,
        "notifications.role.reviewer",
        "notifications.agent.ses-parent",
        "notifications.agent.ses-1",
        "notifications.legion.acme.controller",
        ISSUE,
        "team.deploys",
      ],
      (topic) => topic === "notifications.legion.acme.controller"
    );

    expect(followed).toEqual(["notifications.agent.ses-1", ISSUE, "team.deploys"]);
    expect(replayFollowedSubjects("notifications.agent.ses-1", [])).toEqual([
      "notifications.agent.ses-1",
    ]);
  });

  test("carry the session's own subject once, in the form the host registers", () => {
    // A state an earlier build persisted names the old form; the session follows its own subject
    // under every project now, and the two are one interest.
    const own = ownAgentSubject("ses-1");
    expect(
      replayFollowedSubjects(own, [
        "notifications.agent.ses-1",
        own,
        ISSUE,
        "notifications.agent.ses-2",
      ])
    ).toEqual([own, ISSUE]);
  });

  test("leave out a controller lane even for its holder: the role's claim follows it again", () => {
    const lane = legionControllerNoticeSubject("acme");
    expect(replayFollowedSubjects("notifications.agent.ses-1", [lane])).toEqual([
      "notifications.agent.ses-1",
    ]);
  });

  test("are the persisted topics but agent subjects and role lanes", () => {
    expect(
      rememberedTopics([
        "notifications.agent.ses-1",
        "notifications.agent.ses-parent",
        "notifications.role.reviewer",
        ISSUE,
        "team.deploys",
      ])
    ).toEqual([ISSUE, "team.deploys"]);
    expect(rememberedTopics(undefined)).toEqual([]);
    expect(rememberedTopics([])).toEqual([]);
  });
});

describe("the topics a registration carries", () => {
  const OWN = ownAgentSubject("ses-1");

  test("are the session's own subject, in one form, and every topic the listener would store", () => {
    // A stored row read back holds the old form of the session's own subject beside the one the
    // host registered: the registration carries it once.
    expect(
      registrationTopics(OWN, [
        "notifications.agent.ses-1",
        "notifications.agent.PRIV.ses-1",
        "notifications.dispatch.issue.DSP.DSP-1.>",
      ])
    ).toEqual([OWN, "notifications.dispatch.issue.DSP.DSP-1.>"]);
  });

  test("are the session's own subject and every topic the listener would store for it", () => {
    expect(
      registrationTopics(OWN, [
        "notifications.team.deploys",
        OWN,
        "notifications.dispatch.issue.DSP-1.>",
        "notifications.role.reviewer",
        "team.deploys",
        "notifications.team.deploys",
      ])
    ).toEqual([
      OWN,
      "notifications.team.deploys",
      "notifications.dispatch.issue.DSP-1.>",
      // A role lane is its holder's to register (POST /v1/roles/set) and the listener stores the
      // interest, and a topic outside `notifications.` is no concern of the stream's rules.
      "notifications.role.reviewer",
      "team.deploys",
    ]);
  });

  test("leave out a topic nothing would be delivered on, which would be refused 400", () => {
    // `notifications.team` is the bare base an earlier build expanded `notifications.team.>` to,
    // and the one a stored row or a transcript brings back (INFRA-771 review).
    expect(
      registrationTopics(OWN, ["notifications.team", "notifications.lxspec.probe.all"])
    ).toEqual([OWN]);
  });

  test("leave out a private stream subject that is not this session's, the role's lane apart", () => {
    const lane = legionControllerNoticeSubject("acme");
    expect(
      registrationTopics(OWN, ["notifications.agent.ses-other", lane, "notifications.agent.*"])
    ).toEqual([OWN]);
    expect(registrationTopics(OWN, [lane], [controllerToken("acme")])).toEqual([OWN, lane]);
  });
});

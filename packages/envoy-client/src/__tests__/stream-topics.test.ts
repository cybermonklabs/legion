import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import {
  controllerToken,
  legionControllerNoticeSubject,
  uncapturedTopicWarning,
} from "@legion/contracts";
import {
  controllerNoticeRole,
  reachesPrivateStreamSubject,
  registrationTopics,
  rememberedTopics,
  replayFollowedSubjects,
  warningsForNamedTopics,
} from "../stream-topics";

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
    expect({
      topic,
      roles,
      reaches: reachesPrivateStreamSubject(`notifications.agent.${sessionID}`, topic, roles),
    }).toEqual({ topic, roles, reaches: want });
  }
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
  const OWN = "notifications.agent.ses-1";

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

  test("keep a topic no stream subject this build compiles captures: the listener judges it", () => {
    // `notifications.x.y` is under no subject this build's stream compiles (the listener's own rows
    // use `notifications.x.>` as the subject a newer build adds, and check that bus.StreamCaptures
    // refuses it). During a roll the deployed stream can hold that newer subject, and only the
    // listener reads it: a topic left out here would stay out of the session's consumer filter,
    // and the consumer would move past what is stored on it. `notifications.team` is the bare base
    // an earlier build expanded `notifications.team.>` to, which a stored row or a transcript
    // brings back (INFRA-771 review); it goes too, and the listener answers its warning.
    expect(
      registrationTopics(OWN, [
        "notifications.x.y",
        "notifications.x.>",
        "notifications.team",
        "notifications.lxspec.probe.all",
      ])
    ).toEqual([
      OWN,
      "notifications.x.y",
      "notifications.x.>",
      "notifications.team",
      "notifications.lxspec.probe.all",
    ]);
  });

  test("leave out a private stream subject that is not this session's, the role's lane apart", () => {
    const lane = legionControllerNoticeSubject("acme");
    expect(
      registrationTopics(OWN, ["notifications.agent.ses-other", lane, "notifications.agent.*"])
    ).toEqual([OWN]);
    expect(registrationTopics(OWN, [lane], [controllerToken("acme")])).toEqual([OWN, lane]);
  });
});

describe("the warnings a subscribe tool shows", () => {
  const session = "ses-1";
  const base = uncapturedTopicWarning("notifications.y.z", session);
  const named = uncapturedTopicWarning("notifications.lxspec.probe", session);
  const followed = uncapturedTopicWarning("notifications.team", session);
  const privateSubject =
    "notifications.agent.ses-other was not registered: it reaches a private stream subject";
  const wiring =
    "no GitHub event for acme/site in the stream's retention window; is the App installed there?";
  const answered = (...warnings: string[]) => ({ session_id: session, warnings });

  test("leave out the uncaptured-topic warning of a topic the caller did not name", () => {
    // `notifications.y.z` is the base the client added for `notifications.y.z.>`, and
    // `notifications.team` a topic the session follows that the caller did not name in this call:
    // both stay registered, and the caller, who named neither, is not shown their warnings.
    expect(
      warningsForNamedTopics(answered(base, named, followed), [
        "notifications.y.z.>",
        "notifications.lxspec.probe",
      ])
    ).toEqual([named]);
  });

  test("show the warning of a base the caller named itself, and every warning of another kind", () => {
    expect(warningsForNamedTopics(answered(base), ["notifications.y.z"])).toEqual([base]);
    expect(
      warningsForNamedTopics(answered(privateSubject, wiring), ["notifications.y.z.>"])
    ).toEqual([privateSubject, wiring]);
    expect(warningsForNamedTopics({ session_id: session }, ["notifications.y.z.>"])).toEqual([]);
  });

  test("show a warning that is not the uncaptured-topic text for the answer's session, word for word", () => {
    // The sentence for another session, or a sentence in other words: neither is the warning the
    // listener builds for this answer, so neither is taken for it.
    const otherSession = uncapturedTopicWarning("notifications.y.z", "ses-2");
    const otherWords = "notifications.y.z is registered, but nothing captures it";
    expect(
      warningsForNamedTopics(answered(otherSession, otherWords), ["notifications.y.z.>"])
    ).toEqual([otherSession, otherWords]);
  });
});

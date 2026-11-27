// The rows on how a session gives back a role: the warning when a role's notice subjects are not
// taken out of the registry, and the release of the role claim at `session_shutdown`. They have a
// file of their own, which holds the rows of this subject and stays under 1,000 lines. Their
// `describe` is "envoy OMP extension", the one `envoy.test.ts` uses, so a row's full name is the
// same in every file. Both files share one harness (`../test/extension-harness`).
import { describe, expect, test } from "bun:test";
import { claimEnvoyRole, subscribeLegionNotice } from "@legion/pi-shared/role-claim-bridge";
import { logger } from "@oh-my-pi/pi-utils";
import type { EnvoyModule } from "../test/envoy-module";
import {
  ANY_CLIENT_INSTANCE,
  createPi,
  sessionContext,
  until,
  useExtensionHarness,
} from "../test/extension-harness";
import { busOutageAnswer, response, responseWithRegistration } from "../test/listener-stand-in";

useExtensionHarness();

describe("envoy OMP extension", () => {
  // A role's end takes its notice subjects, and the topics the deployment names for it, out of
  // the registry. The listener answers its bus-outage 503 also after it wrote the record, when
  // only the filter update met a reconnect.
  test("a role's notice subjects whose removal met the bus-outage 503 are warned as that outage, and any other failure as not taken out", async () => {
    const notTakenOut =
      "envoy: a role's notice subjects or topics were not taken out of the registry";
    const rows = [
      {
        answer: () => busOutageAnswer(503),
        warned:
          "envoy: the listener's message bus did not answer a request to take a role's notice subjects or topics out of the registry. The registry may already be without them. The session's next registration sets its consumer's filter to the topics the registry holds.",
      },
      { answer: () => busOutageAnswer(500), warned: notTakenOut },
      {
        answer: (): Response => {
          throw new TypeError("fetch failed");
        },
        warned: notTakenOut,
      },
    ];
    let lane = "";
    let answer = () => response({});
    globalThis.fetch = async (input, init) => {
      const path = new URL(input.toString()).pathname;
      const body = JSON.parse(init?.body?.toString() ?? "{}");
      if (path === "/v1/roles/set") {
        const topics = [`notifications.role.${body.role}`];
        return response({ session_id: "ses_notice", machine_id: "test", dir: "/tmp", topics });
      }
      // The role's own topic is handed back in a request of its own, answered `{}`.
      if (path === "/v1/interests/unsubscribe") {
        return body.topics.includes(lane) ? answer() : response({});
      }
      return responseWithRegistration(input, init, {});
    };
    const warnings: string[] = [];
    const stopSink = logger.registerLogSink((entry) => {
      if (entry.level === "warn" && entry.message.includes("notice subjects")) {
        warnings.push(entry.message);
      }
    });
    try {
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?notice-removal-unanswered"
      );
      const fixture = createPi();
      await envoyExtension(fixture.pi);
      const context = sessionContext("ses_notice");
      await fixture.handlers.get("session_start")?.({}, context);
      for (const [index, row] of rows.entries()) {
        const role = `notice-${index}`;
        lane = `notifications.slack.T01.C${index}.mention`;
        answer = row.answer;
        await claimEnvoyRole("ses_notice", role, context);
        await subscribeLegionNotice("ses_notice", lane, context, role);
        // The role ends: its notice subject is closed and taken out of the registry.
        await claimEnvoyRole("ses_notice", "notice-other", context);
        await until(() => warnings.length > index, `the warning about ${role}'s notices`);
        expect(warnings[index]).toBe(row.warned);
      }
    } finally {
      stopSink();
    }
  });

  // A clean stop gives back the role claim this process holds before it deletes the session's
  // record, so the session's next process claims the role at once rather than being refused it
  // for the stopped process's live window (R16 of dispatch://INFRA-3589). The release names this
  // process, so the listener frees only the claim this process made. The SIGTERM rows, in which
  // Oh My Pi's RPC mode emits no `session_shutdown`, are `stopping-session.test.ts`.
  test("session_shutdown gives back the role this process holds before it deletes the session's record", async () => {
    const requests: string[] = [];
    const releases: unknown[] = [];
    const deletions: string[] = [];
    globalThis.fetch = async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body === undefined ? undefined : JSON.parse(init.body.toString());
      requests.push(`${init?.method ?? "GET"} ${url.pathname}`);
      if (url.pathname === "/v1/interests/unsubscribe") releases.push(body);
      if (url.pathname === "/v1/sessions/ses_shutdown") deletions.push(url.search);
      if (url.pathname === "/v1/roles/set") {
        return response({
          session_id: "ses_shutdown",
          machine_id: "test",
          dir: "/tmp",
          topics: ["notifications.role.reviewer"],
        });
      }
      return responseWithRegistration(input, init, {});
    };
    const { default: envoyExtension }: EnvoyModule = await import(
      "./envoy.ts?shutdown-gives-back-role"
    );
    const fixture = createPi();

    await envoyExtension(fixture.pi);
    const context = sessionContext("ses_shutdown");
    await fixture.handlers.get("session_start")?.({}, context);
    const claimed = await fixture.tools
      .find((tool) => tool.name === "envoy_role_set")
      ?.execute("", { role: "reviewer" }, undefined, undefined, context);
    expect(claimed?.isError).toBeUndefined();
    requests.length = 0;
    await fixture.handlers.get("session_shutdown")?.({}, context);

    expect(requests).toEqual([
      "POST /v1/interests/unsubscribe",
      "DELETE /v1/sessions/ses_shutdown",
    ]);
    // The DELETE names the same process the release does.
    const instance = new URLSearchParams(deletions[0]).get("client_instance_id");
    expect(instance).toEqual(ANY_CLIENT_INSTANCE);
    expect(releases).toEqual([
      {
        session_id: "ses_shutdown",
        topics: ["notifications.role.reviewer"],
        client_instance_id: instance,
      },
    ]);
  });
});

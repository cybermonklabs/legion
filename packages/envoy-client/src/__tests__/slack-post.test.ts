import { afterEach, describe, expect, test } from "bun:test";
import { chmodSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { unshownSettingUrl } from "@legion/contracts/setting-url";
import {
  REDIRECT_CODES,
  redirectCase,
  redirectServers,
  UNNAMED_TARGETS,
} from "@legion/test-fixtures/redirect-servers";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { postSlackReply, SLACK_BOT_TOKEN_SECRET } from "../slack-post";

// A stand-in for the secrets broker's client: it records its arguments and, as the exec form does
// once the broker grants the secret, runs the command after `--` with the secret in its
// environment; FAKE_BROKER=pending or denied answers as the real client does for those states.
const FAKE_AGENT_SECRETS = `#!/bin/sh
printf '%s\\n' "$@" > "$FAKE_ARGS"
case "$FAKE_BROKER" in
  pending)
    echo "agent-secrets: request req-1 is waiting for approval; waiting up to 0s" >&2
    echo "agent-secrets: request req-1 is still waiting for approval; nothing was run. Check it with: agent-secrets status req-1" >&2
    exit 75 ;;
  denied)
    echo "agent-secrets: request req-1 was denied" >&2
    exit 77 ;;
  unenrolled)
    echo "agent-secrets: AGENT_SECRETS_URL is required" >&2
    exit 2 ;;
esac
while [ "$1" != "--" ]; do shift; done
shift
echo "agent-secrets: LEGION_SLACK_BOT_TOKEN from the broker AGENT_SECRETS_URL names (GCP Secret Manager)" >&2
if [ "$FAKE_BROKER" = crash-after-inject ]; then
  echo "the poster's private stderr" >&2
  exit 1
fi
LEGION_SLACK_BOT_TOKEN=\${FAKE_TOKEN:-xoxb-granted} exec "$@"
`;

interface SlackCall {
  readonly path: string;
  readonly authorization: string | null;
  readonly body: unknown;
}

const makeDirectory = temporaryDirectories("slack-post");
const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0)) cleanup();
});

/** A fake Slack Web API whose chat.postMessage answers `answer`, recording every call. */
function fakeSlack(answer: unknown): { readonly url: string; readonly calls: SlackCall[] } {
  const calls: SlackCall[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      calls.push({
        path: new URL(request.url).pathname,
        authorization: request.headers.get("authorization"),
        body: await request.json(),
      });
      return Response.json(answer);
    },
  });
  cleanups.push(() => server.stop(true));
  return { url: `http://127.0.0.1:${server.port}/api`, calls };
}

/** A hostile stand-in for Slack's Web API: every answer carries back the Authorization header it
 * was sent, as `answer` builds it from that header. Returns its URL and how many posts it saw. */
function reflectingSlack(answer: (authorization: string) => Response): {
  readonly url: string;
  readonly posts: () => number;
} {
  let posts = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch(request) {
      posts += 1;
      return answer(request.headers.get("authorization") ?? "");
    },
  });
  cleanups.push(() => server.stop(true));
  return { url: `http://127.0.0.1:${server.port}/api`, posts: () => posts };
}

/** What postSlackReply hands its caller, the session's tool result: its answer or its error. */
async function outcome(env: Record<string, string>): Promise<string> {
  try {
    return JSON.stringify(await postSlackReply(reply, { env }));
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
}

/** An environment whose PATH finds the fake broker client first, and the file it writes its
 * arguments to. The client grants `token`, `xoxb-granted` when none is given. */
function brokerEnvironment(mode: string, slackURL: string, token?: string) {
  const dir = makeDirectory();
  const client = path.join(dir, "agent-secrets");
  writeFileSync(client, FAKE_AGENT_SECRETS);
  chmodSync(client, 0o755);
  const args = path.join(dir, "args");
  return {
    args,
    env: {
      PATH: `${dir}${path.delimiter}${process.env.PATH ?? ""}`,
      FAKE_ARGS: args,
      FAKE_BROKER: mode,
      SLACK_POST_API_URL: slackURL,
      ...(token === undefined ? {} : { FAKE_TOKEN: token }),
    } as Record<string, string>,
  };
}

const reply = { channel: "C00000000", threadTs: "1700000000.000100", text: "Filed as ACME-12." };

describe("postSlackReply", () => {
  test("posts through the broker's exec form with the granted token and answers Slack's ts", async () => {
    const slack = fakeSlack({ ok: true, channel: "C00000000", ts: "1700000000.000200" });
    const { env, args } = brokerEnvironment("granted", slack.url);

    const posted = await postSlackReply(reply, { env });

    expect(posted).toEqual({
      channel: "C00000000",
      threadTs: "1700000000.000100",
      ts: "1700000000.000200",
    });
    expect(slack.calls).toEqual([
      {
        path: "/api/chat.postMessage",
        authorization: "Bearer xoxb-granted",
        body: { channel: "C00000000", thread_ts: "1700000000.000100", text: "Filed as ACME-12." },
      },
    ]);
    const argv = readFileSync(args, "utf-8").trimEnd().split("\n");
    expect(argv.slice(0, argv.indexOf("--"))).toEqual([
      SLACK_BOT_TOKEN_SECRET,
      "--reason",
      "slack_post: reply in Slack channel C00000000, thread 1700000000.000100",
      "--wait",
      "0s",
    ]);
    expect(argv.slice(argv.indexOf("--") + 1, argv.indexOf("--") + 3)).toEqual(["bun", "-e"]);
  });

  test("a pending grant posts nothing and names the broker's state", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("pending", slack.url);

    const failure = postSlackReply(reply, { env });

    await expect(failure).rejects.toThrow(
      "the secrets broker has not granted LEGION_SLACK_BOT_TOKEN: its request is waiting for approval"
    );
    await expect(failure).rejects.toThrow("nothing was posted");
    await expect(failure).rejects.toThrow("agent-secrets: request req-1 is still waiting");
    expect(slack.calls).toEqual([]);
  });

  // The broker's client exits without reading the reply when it runs no poster, so a write to its
  // stdin can meet a closed pipe. A reply larger than the pipe holds makes that certain; the
  // outcome is still the broker's state, never a stray EPIPE.
  test("a broker that exits before reading the reply reports its state, not a broken pipe", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("pending", slack.url);

    await expect(postSlackReply({ ...reply, text: "x".repeat(1 << 20) }, { env })).rejects.toThrow(
      "the secrets broker has not granted LEGION_SLACK_BOT_TOKEN: its request is waiting for approval"
    );
    expect(slack.calls).toEqual([]);
  });

  test("a denied grant posts nothing and names the broker's state", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("denied", slack.url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "the secrets broker denied LEGION_SLACK_BOT_TOKEN (agent-secrets: request req-1 was denied); nothing was posted"
    );
    expect(slack.calls).toEqual([]);
  });

  test("a poster that dies after the injection line may have posted, without quoting its stderr", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("crash-after-inject", slack.url);

    const said = await outcome(env);

    expect(said).toContain(
      "slack_post's poster exited 1 without an answer: the reply may have been posted"
    );
    expect(said).not.toContain("nothing was posted");
    expect(said).not.toContain("agent-secrets: LEGION_SLACK_BOT_TOKEN");
    expect(said).not.toContain("the poster's private stderr");
    expect(slack.calls).toEqual([]);
  });

  test("any other broker failure posts nothing and quotes the client", async () => {
    const slack = fakeSlack({ ok: true, ts: "1.2" });
    const { env } = brokerEnvironment("unenrolled", slack.url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "the secrets broker did not release LEGION_SLACK_BOT_TOKEN (agent-secrets: AGENT_SECRETS_URL is required); nothing was posted"
    );
    expect(slack.calls).toEqual([]);
  });

  // A reply goes to people in Slack, who cannot open an address inside the cluster: every http(s)
  // URL in the reply, Slack's <url|text> form included, is parsed and its host checked, and a reply
  // linking one is refused before the broker is asked or Slack is called, naming the link. Inside
  // means a Service name (.svc, a custom cluster domain after .svc, .cluster.local, a single label
  // in a URL), a private or loopback IP address, or, while DISPATCH_PUBLIC_URL names another host,
  // the host of DISPATCH_URL itself, whatever form the deployment gave it. Text with no scheme is
  // judged as written, so decimals, versions and the word "Dispatch" post.
  describe("refuses a reply linking an address inside the cluster, asking nothing of the broker", () => {
    const internal = "dispatch.example-ns.svc.cluster.local";
    const rows: [string, string, Record<string, string>?][] = [
      ["an FQDN Service link", `Filed as ACME-12: http://${internal}:8766/issues/ACME-12`],
      ["upper case", `See HTTP://${internal.toUpperCase()}:8766/issues/ACME-12`],
      ["Slack's link form", `Filed as <http://${internal}:8766/issues/ACME-12|ACME-12>`],
      [
        "Slack's link form on a .svc host",
        "Filed as <http://dispatch.example-ns.svc:8766/issues/ACME-12|ACME-12>",
      ],
      ["userinfo", `See http://u:p@${internal}/issues/ACME-12`],
      ["Slack-escaped angle brackets", `See &lt;http://${internal}:8766/issues/ACME-12&gt;`],
      ["a trailing-dot FQDN", `See http://${internal}.:8766/issues/ACME-12`],
      ["a ClusterIP", "See http://10.96.12.34:8766/issues/ACME-12"],
      ["an IPv6 literal", "See http://[fd00:10:96::1234]:8766/issues/ACME-12"],
      ["a loopback address", "See http://127.0.0.1:8766/issues/ACME-12"],
      ["a single-label Service name", "See http://dispatch:8766/issues/ACME-12"],
      [
        "percent-encoded dots",
        "See http://dispatch%2Eexample-ns%2Esvc%2Ecluster%2Elocal:8766/issues/ACME-12",
      ],
      ["a bare origin ending a sentence", `Dispatch is at http://${internal}:8766.`],
      ["a bare origin in parentheses", `Dispatch (http://${internal}:8766) has it.`],
      ["a bare origin before a query", `See http://${internal}:8766?x=1`],
      ["a bare origin before a fragment", `See http://${internal}:8766#top`],
      [
        "a custom cluster domain",
        "See http://dispatch.example-ns.svc.corp.internal/issues/ACME-12",
      ],
      ["a host with no scheme", `See ${internal}:8766/issues/ACME-12`],
      [
        "DISPATCH_URL's short <svc>.<ns> host while the dashboard is elsewhere",
        "See http://dispatch.example-ns:8766/issues/ACME-12",
        {
          DISPATCH_URL: "http://dispatch.example-ns:8766",
          DISPATCH_PUBLIC_URL: "https://dispatch.example.org",
        },
      ],
      ["an IPv4-mapped ClusterIP", "See http://[::ffff:10.96.12.34]:8766/issues/ACME-12"],
      ["an IPv4-mapped loopback address", "See http://[::ffff:127.0.0.1]:8766/issues/ACME-12"],
      ["a two-label Service host with no scheme", "See dispatch.svc:8766/issues/ACME-12"],
      ["a parenthesized .svc link before a full stop", "See (http://dispatch.example-ns.svc)."],
      [
        "DISPATCH_URL's two-label host with no scheme while the dashboard is elsewhere",
        "See dispatch.example-ns:8766/issues/ACME-12",
        {
          DISPATCH_URL: "http://dispatch.example-ns:8766",
          DISPATCH_PUBLIC_URL: "https://dispatch.example.org",
        },
      ],
      [
        "DISPATCH_URL's host in parentheses before a full stop while the dashboard is elsewhere",
        "See (http://dispatch.example-ns).",
        {
          DISPATCH_URL: "http://dispatch.example-ns:8766",
          DISPATCH_PUBLIC_URL: "https://dispatch.example.org",
        },
      ],
      [
        "DISPATCH_URL's single-label host with no scheme while the dashboard is elsewhere",
        "See dispatch:8766/issues/ACME-12",
        {
          DISPATCH_URL: "http://dispatch:8766",
          DISPATCH_PUBLIC_URL: "https://dispatch.example.org",
        },
      ],
      ["a ClusterIP with no scheme", "See 10.96.12.34:8766/issues/ACME-12"],
      ["a bracketed IPv6 literal with no scheme", "See [fd00:10:96::1234]:8766/issues/ACME-12"],
      ["a mapped IPv6 literal with no scheme", "See [::ffff:10.96.12.34]:8766/issues/ACME-12"],
      ["loopback in IPv4 shorthand", "See http://127.1:8766/issues/ACME-12"],
      ["loopback as one hex number", "See http://0x7f000001:8766/issues/ACME-12"],
      ["the unspecified IPv6 address", "See http://[::]:8766/issues/ACME-12"],
    ];
    for (const [name, text, extra] of rows) {
      test(name, async () => {
        const slack = fakeSlack({ ok: true, ts: "1.2" });
        const { env, args } = brokerEnvironment("granted", slack.url);
        await expect(
          postSlackReply({ ...reply, text }, { env: { ...env, ...extra } })
        ).rejects.toThrow(
          "nobody in Slack can open; link Dispatch's dashboard instead (dispatch prints it when DISPATCH_PUBLIC_URL is set); nothing was posted"
        );
        expect(slack.calls).toEqual([]);
        expect(() => readFileSync(args)).toThrow();
      });
    }

    const allowed: [string, string, Record<string, string>?][] = [
      ["the public dashboard", "Filed as ACME-12: https://dispatch.example.org/issues/ACME-12"],
      ["a path that merely ends in .svc", "See http://example.org/docs.svc"],
      ["a dotted word in prose", "Fixed in README.md, e.g. the setup section."],
      [
        "DISPATCH_URL's host when it is the dashboard too",
        "See https://dispatch.example.org/issues/ACME-12",
        { DISPATCH_URL: "https://dispatch.example.org" },
      ],
      ["a fraction of a CPU", "Each pod asks for 0.5 CPU."],
      ["a duration in minutes", "CI ran for 10.5 minutes on ACME-12."],
      ["a latency", "Median wait was 127.3 ms."],
      ["a price", "It costs $0.42 a run."],
      ["a p-value", "The difference holds, p < 0.05."],
      ["a package version", "pi-envoy 0.31.0 is installed."],
      ["Legion's own version", "This deployment runs Legion 10.4.0."],
      ["a tagged version", "Released as v10.4.1."],
      ["two dotted numbers", "The prefix 192.168 is private."],
      ["a dispatch:// reference", "Tracked in dispatch://ACME-12."],
      [
        "the word Dispatch before a colon while DISPATCH_URL is one label",
        "Dispatch: filed ACME-12, https://dispatch.example.org/issues/ACME-12",
        {
          DISPATCH_URL: "http://dispatch:8766",
          DISPATCH_PUBLIC_URL: "https://dispatch.example.org",
        },
      ],
      [
        "the word Dispatch before a slash while DISPATCH_URL is one label",
        "Dispatch/Legion tracks it as ACME-12.",
        {
          DISPATCH_URL: "http://dispatch:8766",
          DISPATCH_PUBLIC_URL: "https://dispatch.example.org",
        },
      ],
      ["a public IPv4-mapped address", "See http://[::ffff:8.8.8.8]/"],
      ["a public IPv6 address", "See http://[2001:db8::1]/"],
    ];
    for (const [name, text, extra] of allowed) {
      test(`posts ${name}`, async () => {
        const slack = fakeSlack({ ok: true, channel: "C00000000", ts: "1700000000.000200" });
        const { env } = brokerEnvironment("granted", slack.url);
        await expect(
          postSlackReply({ ...reply, text }, { env: { ...env, ...extra } })
        ).resolves.toMatchObject({
          ts: "1700000000.000200",
        });
        expect(slack.calls).toHaveLength(1);
      });
    }
  });

  test("Slack's refusal comes back verbatim", async () => {
    const slack = fakeSlack({ ok: false, error: "not_in_channel" });
    const { env } = brokerEnvironment("granted", slack.url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "Slack refused chat.postMessage: not_in_channel"
    );
    expect(slack.calls).toHaveLength(1);
  });

  test("a Slack that does not answer is a failure that says the reply may have been posted", async () => {
    // A port nothing listens on: the poster's request fails after the grant.
    const closed = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response() });
    const url = `http://127.0.0.1:${closed.port}/api`;
    closed.stop(true);
    const { env } = brokerEnvironment("granted", url);

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "the reply may have been posted, and posting it again may put it in the thread twice"
    );
  });

  test("a Slack API override that is not https or a loopback host is refused before the broker is asked", async () => {
    const { env, args } = brokerEnvironment("granted", "http://slack.example/api");

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "SLACK_POST_API_URL must be an https URL, or http on a loopback host"
    );
    expect(() => readFileSync(args)).toThrow();
  });

  test("a session without the broker's client says so and posts nothing", async () => {
    const env = { PATH: makeDirectory("empty") };

    await expect(postSlackReply(reply, { env })).rejects.toThrow(
      "agent-secrets is not on PATH: slack_post takes the bot token from the secrets broker, which this session is not enrolled with; nothing was posted"
    );
  });

  // The token lives in the poster alone. An answer that carries it back, whatever field it rides
  // in, never reaches the session: the poster passes on only Slack's own error codes and a ts and
  // channel shaped as Slack's, never a body, and redacts the token from all it prints. The token
  // holds capitals, so a part that the URL parser lower-cases is not missed by the comparison.
  describe("never hands the session the token an answer carries back", () => {
    // Joined at run time: written whole, a made-up bot token reads to a secret scanner as a real
    // one and stops the push.
    const token = ["xoxb", "Made", "Up", "UPPER", "Token"].join("-");
    const cases: [string, (authorization: string) => Response, string][] = [
      [
        "in an error page",
        (authorization) =>
          new Response(`<html>bad gateway ${authorization}</html>`, { status: 502 }),
        "chat.postMessage answered HTTP 502 with a body that is not Slack's JSON: the reply may have been posted",
      ],
      [
        "as Slack's error code",
        (authorization) => Response.json({ ok: false, error: authorization }),
        "Slack refused chat.postMessage with an error code slack_post does not pass on",
      ],
      [
        "as the posted ts",
        (authorization) => Response.json({ ok: true, channel: "C00000000", ts: authorization }),
        "chat.postMessage answered ok with a ts or channel that is not Slack's: the reply may have been posted",
      ],
      [
        "as the posted channel",
        (authorization) =>
          Response.json({ ok: true, channel: authorization, ts: "1700000000.000200" }),
        "chat.postMessage answered ok with a ts or channel that is not Slack's: the reply may have been posted",
      ],
      [
        // The Location is the answer's too: a host it names that is not the API URL's own is
        // never named back, since a lower-cased host can carry the token past the redaction.
        "as a redirect's host",
        (authorization) =>
          new Response(null, {
            status: 302,
            headers: { Location: `https://${authorization.split(" ")[1]}.example/api` },
          }),
        "chat.postMessage answered HTTP 302, redirecting to another host, and slack_post follows no redirect",
      ],
    ];
    for (const [name, answer, want] of cases) {
      test(name, async () => {
        const slack = reflectingSlack(answer);
        const { env } = brokerEnvironment("granted", slack.url, token);

        const said = await outcome(env);

        expect(slack.posts()).toBe(1);
        expect(said.toLowerCase()).not.toContain(token.toLowerCase());
        expect(said).toContain(want);
      });
    }

    // A token whose shape is a Slack channel id's passes the poster's check of the channel, so
    // the cut of the token from the poster's line is the one guard left: the reply's channel
    // reaches the session as `[redacted]`. The token is made for this run.
    test("as the posted channel, in a channel id's shape, which only the cut keeps back", async () => {
      const shaped = `C${crypto.randomUUID().replaceAll("-", "").toUpperCase().slice(0, 24)}`;
      const slack = reflectingSlack((authorization) =>
        Response.json({ ok: true, channel: authorization.split(" ")[1], ts: "1700000000.000200" })
      );
      const { env } = brokerEnvironment("granted", slack.url, shaped);

      const said = await outcome(env);

      expect(slack.posts()).toBe(1);
      expect(said).not.toContain(shaped);
      expect(JSON.parse(said)).toEqual({
        channel: "[redacted]",
        threadTs: "1700000000.000100",
        ts: "1700000000.000200",
      });
    });
  });

  // The 30 s limit is a hang guard, not a budget: the row stands a fake Slack and a broker
  // environment up once per refusal code, four times over, and bun's unwritten 5,000 ms per-test
  // default is a limit on that work rather than on a hang. Measured in a 2-CPU pod: 1.34 s idle;
  // under 16 busy loops (8 x nproc), 4.02 s at most in a whole-suite run and 2.84 s with this file
  // alone. It fires only if a post never answers.
  test("a Slack error code it knows comes back as Slack spelled it", async () => {
    for (const code of ["channel_not_found", "is_archived", "ratelimited", "invalid_auth"]) {
      const slack = fakeSlack({ ok: false, error: code });
      const { env } = brokerEnvironment("granted", slack.url);
      expect(await outcome(env)).toBe(`Slack refused chat.postMessage: ${code}`);
    }
  }, 30_000);

  // fetch drops `Authorization` on a redirect to another origin, so the token stays behind, but it
  // sends a 307's or 308's reply on, and a post that follows takes the other origin's answer as
  // Slack's. No post follows a redirect: the 3xx is the answer, which names where it points, and
  // the other origin receives nothing. One case for each code, so a post that follows again fails
  // at each. The 30 s limit is the hang guard of the test above: a post through the broker's
  // stand-in and a `bun -e` poster.
  for (const code of REDIRECT_CODES) {
    test(redirectCase(code), async () => {
      const servers = redirectServers({
        atElsewhere: () =>
          Response.json({ ok: true, channel: "C00000000", ts: "1700000000.000200" }),
      });
      cleanups.push(() => servers.stop());
      servers.code = code;
      const { env } = brokerEnvironment("granted", `${servers.origin}/api`);
      const said = await outcome(env);
      expect(servers.received, `the other origin after a ${code}`).toEqual([]);
      expect(said).toBe(
        `chat.postMessage answered HTTP ${code}, redirecting to ${servers.elsewhere}, and slack_post follows no redirect, since the post carries the bot token: nothing was sent there; SLACK_POST_API_URL must name the Web API's own address, or be unset for Slack's`
      );
    }, 30_000);
  }

  // A poster that read a redirect's body before its status would wait on a body that never ends,
  // and refuse nothing until the post's own limit; the test's limit is below that.
  test("a redirect whose body never ends is refused naming its status, before any of its body is read", async () => {
    const servers = redirectServers();
    cleanups.push(() => servers.stop());
    servers.endlessBody = true;
    const { env } = brokerEnvironment("granted", `${servers.origin}/api`);
    expect(await outcome(env)).toBe(
      `chat.postMessage answered HTTP 307, redirecting to ${servers.elsewhere}, and slack_post follows no redirect, since the post carries the bot token: nothing was sent there; SLACK_POST_API_URL must name the Web API's own address, or be unset for Slack's`
    );
  }, 8_000);

  // A redirect within the Web API URL's own address says to correct its path, which the post puts
  // before /chat.postMessage; one that names nowhere is not named as the API's own address; a host
  // longer than any host name is not the API URL's host, so it is another host.
  test("a redirect to another path of the same address, to no target, or to an overlong host is refused saying so", async () => {
    const servers = redirectServers();
    cleanups.push(() => servers.stop());
    const { env } = brokerEnvironment("granted", `${servers.origin}/api`);
    const said: string[] = [];
    for (const { location } of UNNAMED_TARGETS) {
      servers.location = location;
      said.push(await outcome(env));
    }
    expect(said).toEqual(
      UNNAMED_TARGETS.map(({ where, sameAddress }) => {
        const shown =
          sameAddress || where === "naming no target" ? where : "redirecting to another host";
        const fix = sameAddress
          ? 'correct the path of SLACK_POST_API_URL, or unset it for Slack\'s; a missing or an extra "/" is the usual cause'
          : "SLACK_POST_API_URL must name the Web API's own address, or be unset for Slack's";
        return `chat.postMessage answered HTTP 307, ${shown}, and slack_post follows no redirect, since the post carries the bot token: nothing was sent there; ${fix}`;
      })
    );
  }, 30_000);

  // The override may carry a user and a password; the refusal of an http URL that is not on a
  // loopback host names no part of it, whatever the case of its scheme. The password is made for
  // this run.
  test("a refused Slack API override is named with no user and no password", async () => {
    const password = crypto.randomUUID();
    const said: string[] = [];
    for (const scheme of ["http", "HTTP"]) {
      const { env } = brokerEnvironment(
        "granted",
        `${scheme}://made-up-user:${password}@slack.example/api`
      );
      said.push(await outcome(env));
    }
    expect(said).toEqual(
      Array(2).fill(
        "SLACK_POST_API_URL must be an https URL, or http on a loopback host; nothing was posted"
      )
    );
  });

  // An override the URL parser does not read as an http or https URL with a host is refused
  // naming its length and the cause the one function found: that reading cannot be trusted to
  // have found a password or a token, since `user:password@host` parses with `user:` as its scheme
  // and the password in its path. So is one with a `/` in its user part, which the parser reads as
  // a host taken from the user part. The values are made for this run.
  test("a Slack API override that is no http or https URL with a host is named by its length and cause", async () => {
    const password = crypto.randomUUID();
    const refused = [
      `made-up value ${crypto.randomUUID()}`,
      `made-up-user:${password}@slack.example/api`,
      `mailto:made-up-user:${password}@slack.example`,
      `//made-up-user:${password}@slack.example/api`,
      "   ",
      `ftp://made-up-user:${password}@slack.example/api`,
      `https://MadeUpTok3n/${password}@slack.example/api`,
    ];
    const said: string[] = [];
    for (const value of refused) {
      const { env } = brokerEnvironment("granted", value);
      said.push(await outcome(env));
    }
    // Each refused value is named in the one function's words.
    expect(said).toEqual(
      refused.map(
        (value) =>
          `SLACK_POST_API_URL must be an https URL, or http on a loopback host: ${unshownSettingUrl(value)}; nothing was posted`
      )
    );
  });
});

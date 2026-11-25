// packages/cipher-client/scripts/print-trade.ts
// One trade, as a Dispatch or Envoy client makes it, for scripts/project-secrets-check.sh: it builds
// the client brokerTokenClient builds from this process's environment (AGENT_SECRETS_URL, the
// machine identity, a person's sign-in on this machine, AGENT_SECRETS_SESSION and
// AGENT_SECRETS_PROJECTS), trades once, and asks the broker's introspection route
// (/v1/oauth/introspect) what the token it got names, as Dispatch and the listener ask it.
//
// It prints one JSON line, {"active", "person", "service", "project_scope", "projects"}, and never
// the token: what the check reads is which projects the broker put in the token and under which
// scope, and the token itself stays in this process. Only an account the broker lists in
// BROKER_API_ACCOUNTS may introspect, so the bearer is the token in the file
// AGENT_SECRETS_ACCOUNT_TOKEN_FILE names (on dev-stack.sh, the caller "dispatch"'s).
//
// Exit codes: 2, the environment makes no client or names no account token, and nothing was
// traded; 3, the broker refused the trade, its own words on standard error; 4, the trade was
// answered and the introspection was not, its status and words on standard error.
import { readFile } from "node:fs/promises";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { joinUrl } from "@legion/contracts/url-join";
import { brokerTokenClient } from "../src/environment";

const ACCOUNT_TOKEN_FILE_VAR = "AGENT_SECRETS_ACCOUNT_TOKEN_FILE";
const INTROSPECT_PATH = "/v1/oauth/introspect";

function fail(code: number, message: string): never {
  process.stderr.write(`print-trade: ${message}\n`);
  process.exit(code);
}

const accountFile = (process.env[ACCOUNT_TOKEN_FILE_VAR] ?? "").trim();
if (accountFile === "") {
  fail(
    2,
    `${ACCOUNT_TOKEN_FILE_VAR} names no account token, so the trade's introspection could not be read: name the file of an account the broker lists in BROKER_API_ACCOUNTS`
  );
}
const made = await brokerTokenClient(process.env);
if (made?.client === undefined) {
  fail(
    2,
    "this environment makes no broker client: set AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY"
  );
}
const account = (await readFile(accountFile, "utf8")).trim();
let accessToken: string;
try {
  accessToken = (await made.client.token()).accessToken;
} catch (error) {
  fail(3, error instanceof Error ? error.message : String(error));
}
const brokerUrl = withoutTrailingSlashes((process.env.AGENT_SECRETS_URL ?? "").trim());
const answer = await fetch(joinUrl(brokerUrl, INTROSPECT_PATH).href, {
  method: "POST",
  headers: {
    Authorization: `Bearer ${account}`,
    "Content-Type": "application/x-www-form-urlencoded",
  },
  body: new URLSearchParams({ token: accessToken }).toString(),
});
const body = await answer.text();
if (!answer.ok) {
  fail(4, `the broker answered the introspection ${answer.status}: ${body.trim()}`);
}
const read = JSON.parse(body) as Record<string, unknown>;
const text = (key: string): string => (typeof read[key] === "string" ? (read[key] as string) : "");
process.stdout.write(
  `${JSON.stringify({
    active: read.active === true,
    person: text("person"),
    service: text("service"),
    project_scope: text("project_scope"),
    projects: Array.isArray(read.projects) ? read.projects : [],
  })}\n`
);

// Check k7's slack_post poster: bun, the Bearer a Slack app's token goes in, and auth.test.
// The real Slack refuses the check's fake token, and that refusal is what the check reads off
// the line this prints. Slack's own answer is JSON with an `ok` field and prints as Slack wrote
// it; anything else was written by something in Slack's place, which can echo the request back,
// the bearer with it, so it is named in one line with the bearer redacted (`bodyForText`).
import { bodyForText } from "../../../envoy-client/src/excerpt";

const token = process.env.SLACK_BOT_TOKEN;
const answer = await fetch("https://slack.com/api/auth.test", {
  method: "POST",
  headers: {
    authorization: `Bearer ${token}`,
    "content-type": "application/json",
  },
});
const text = await answer.text();
let slacks = false;
try {
  const parsed: unknown = JSON.parse(text);
  slacks = typeof parsed === "object" && parsed !== null && "ok" in parsed;
} catch {
  // Not JSON, so not Slack's: named below.
}
console.log(
  answer.status,
  slacks ? text.slice(0, 200) : bodyForText(text, token ?? "", "Slack's JSON")
);

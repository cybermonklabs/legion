import { LEGION_NOTICE_TOPIC_PREFIX, slackSubject, slackThreadSubject } from "./subject";

/**
 * A private project's Slack lane: the listener's Slack intake publishes an event of a channel that
 * exactly one private project lists (its `slack_channels`, read fresh from the privacy source) on
 * that project's own lane under the Legion daemon's family, `notifications.legion.<token>.slack.…`,
 * in place of `notifications.slack.…`, so the relay gives it only to a token that sees the project
 * (`subjectProject` reads the lane's token as the project). The Go twins are
 * packages/envoy/internal/contracts/private_slack.go.
 */

const SLACK_SUBJECT_PREFIX = "notifications.slack.";

/** The private Slack lane of `project` for a Slack subject's tail. */
function privateSlackLane(project: string, tail: string): string {
  return `${LEGION_NOTICE_TOPIC_PREFIX}${project.toLowerCase()}.slack.${tail}`;
}

/** `notifications.legion.<token>.slack.<team>.<channel>.<kind>`, the token the key in lower case. */
export function privateSlackSubject(
  project: string,
  team: string,
  channel: string,
  kind: string
): string {
  return privateSlackLane(
    project,
    slackSubject(team, channel, kind).slice(SLACK_SUBJECT_PREFIX.length)
  );
}

/** The thread copy's lane: `notifications.legion.<token>.slack.<team>.<channel>.thread.<ts>.<kind>`. */
export function privateSlackThreadSubject(
  project: string,
  team: string,
  channel: string,
  threadTs: string,
  kind: string
): string {
  return privateSlackLane(
    project,
    slackThreadSubject(team, channel, threadTs, kind).slice(SLACK_SUBJECT_PREFIX.length)
  );
}

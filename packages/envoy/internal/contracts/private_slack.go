package contracts

import "strings"

// A private project's Slack lane: the listener's Slack intake publishes an event of a channel
// that exactly one private project lists (its slack_channels, read fresh from the privacy source)
// on that project's own lane under the Legion daemon's family, notifications.legion.<token>.slack.…,
// in place of notifications.slack.…, so the relay gives it only to a token that sees the project
// (SubjectProject reads the lane's token as the project). Nothing of such an event is published on
// notifications.slack.>. The TypeScript twins are packages/contracts/src/private-slack.ts.

// slackSubjectPrefix begins every Slack subject the intake writes (SlackSubject,
// SlackThreadSubject).
const slackSubjectPrefix = "notifications.slack."

// privateSlackLane is the private Slack lane's subject of project for a Slack subject's tail: the
// Legion family's notice lane of the project's token, then slack. and the tail.
func privateSlackLane(project, tail string) string {
	return LegionNoticeTopicPrefix + strings.ToLower(project) + ".slack." + tail
}

// PrivateSlackSubject is the subject of a Slack event in a private project's channel:
// notifications.legion.<token>.slack.<team>.<channel>.<kind>, the token the project's key in
// lower case, as the Legion daemon's lanes write it.
func PrivateSlackSubject(project, team, channel, kind string) string {
	return privateSlackLane(project, strings.TrimPrefix(SlackSubject(team, channel, kind), slackSubjectPrefix))
}

// PrivateSlackThreadSubject is the subject of the thread copy of such an event:
// notifications.legion.<token>.slack.<team>.<channel>.thread.<ts>.<kind>.
func PrivateSlackThreadSubject(project, team, channel, threadTs, kind string) string {
	return privateSlackLane(project, strings.TrimPrefix(SlackThreadSubject(team, channel, threadTs, kind), slackSubjectPrefix))
}

// PrivateSlackTopic moves a subject the Slack intake wrote (SlackSubject or SlackThreadSubject) to
// project's private Slack lane, and reports false for a subject that is no Slack subject.
func PrivateSlackTopic(project, slackSubject string) (string, bool) {
	tail, found := strings.CutPrefix(slackSubject, slackSubjectPrefix)
	if !found || tail == "" {
		return "", false
	}
	return privateSlackLane(project, tail), true
}

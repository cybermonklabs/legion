package agent

// quotaShare is the agent's share of the chat deployment's tokens a minute
// (DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE), which its going.Pacer paces its runs' model calls to:
// its runs take 75% of the quota, and the server's short answers the rest, each process with a
// pacer of its own, so a deployment with a smaller quota keeps the same split.
const quotaShare = 0.75

// charsPerToken is how many characters of a request count as one token (Level.estimate): the
// rough rule the server's answerer paces its own share with (going.charsPerToken).
const charsPerToken = 4

# Envoy from a subagent

Read this as a `task` subagent before you send, publish or give a peer your address.

- A `task` subagent registers no session of its own: `envoy_whoami`'s `session_id` is the parent's, and the parent forwards a reply to you with a `write` to your `agent://<id>` address, where `<id>` is the id your harness gave you.
  Never give a peer the `subagent` field of `envoy_whoami`: it is not an address.
- Start the message with who you are and where a reply reaches you ("From <name>, a subagent of session <session_id>; a reply to <session_id> reaches me", with `envoy_whoami`'s `session_id`, the parent's, in both places), because the recipient sees only the parent as sender.
  An empty `session_id` (the process took no Envoy identity, or it can no longer place the session that spawned you) means your messages carry no sender at all: name yourself in the body.
- A publish to a role your parent holds reaches nobody, because the listener drops a role message whose holder is its source. `write` to the parent's `agent://<id>` address instead, the id your harness names for the agent that spawned you.
  `envoy_send` to any other session works.

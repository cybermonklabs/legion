:::requirement{#sessions-keep-their-messages kind="behavior"}
Envoy keeps each message of a session until the session reads it.
:::

:::requirement{#a-session-reads-its-messages-in-order kind="behavior" parent="sessions-keep-their-messages"}
A session reads its messages in the order that their senders wrote them.
:::

:::check{#messages-come-in-the-order-of-their-writes serves="a-session-reads-its-messages-in-order" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestMessagesComeInTheOrderOfTheirWrites" job="go-checks"}
Claim: Two messages that one sender writes reach the session in the order of the writes.
Procedure: The program writes two messages from one sender and reads the session.
It must find them in the order of the writes.
Failing example: A listener that delivers the newest message first.
:::

:::requirement{#envoy-delivers-messages kind="behavior"}
Envoy delivers each message that a sender writes.
:::

:::check{#a-sent-message-reaches-a-listener serves="envoy-delivers-messages" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestASentMessageReachesAListener" job="go-checks"}
Claim: A message that a sender writes reaches a listener that is connected.
Procedure: The program connects one listener and writes one message.
It must find the message at the listener.
Failing example: A sender that drops each message.
:::

:::requirement{#a-message-reaches-a-late-session kind="behavior" parent="envoy-delivers-messages"}
A session that connects after a message was written gets that message.
:::

:::check{#a-late-session-gets-the-message serves="a-message-reaches-a-late-session" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestALateSessionGetsTheMessage" job="go-checks"}
Claim: A session that connects after a write gets the message of that write.
Procedure: The program writes one message and then connects one session.
It must find the message in the session.
Failing example: A listener that delivers only the messages that arrive after a session connects.
:::

:::requirement{#a-message-survives-a-restart kind="behavior" parent="envoy-delivers-messages"}
A message that Envoy stored before a restart reaches its session after the restart.
:::

:::check{#a-restart-keeps-the-message serves="a-message-survives-a-restart" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestARestartKeepsTheMessage" job="go-checks"}
Claim: A stored message reaches its session after the listener starts again.
Procedure: The program stores one message, starts the listener again and reads the session.
It must find the message one time.
Failing example: A listener that deletes its stored messages when it starts.
:::

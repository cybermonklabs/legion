:::requirement{#relay-answers-requests kind="behavior"}
The relay answers each request that it receives.
:::

:::requirement{#relay-answers-in-time kind="behavior" parent="relay-answers-requests"}
The relay answers a request within the time that the request names.
:::

:::check{#relay-answers-within-the-limit serves="relay-answers-in-time" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestTheRelayAnswersWithinTheLimit" job="go-checks" time_limit="24h0m0s"}
Claim: The relay answers a request that names one second within one second.
Procedure: The program sends one request that names one second.
It must get the answer within one second.
Failing example: A relay that waits two seconds before each answer.
:::

:::requirement{#relay-answers-on-each-platform kind="behavior" parent="relay-answers-requests"}
The relay answers each request on Linux machines and on Arm machines.
:::

:::check{#relay-answers-on-linux-and-arm serves="relay-answers-on-each-platform" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestTheRelayAnswersOnEachPlatform" job="go-checks-platforms"}
Claim: The relay answers one request on each machine of the job.
Procedure: The program sends one request to the relay and reads the answer.
It must get one answer.
Failing example: A relay that stops before it answers.
:::

:::requirement{#relay-answers-once kind="behavior" parent="relay-answers-requests"}
The relay sends one answer for each request.
:::

:::check{#relay-answers-once-per-request serves="relay-answers-once" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestTheRelayAnswersOnce" job="go-checks"}
Claim: The relay sends one answer to a request that it receives two times.
Procedure: The program sends one request two times and counts the answers.
It must count one answer.
Failing example: A relay that answers each copy of a request.
:::

:::requirement{#relay-refuses-a-bad-request kind="behavior" parent="relay-answers-requests"}
The relay refuses a request that it cannot read.
:::

:::check{#relay-refuses-a-malformed-request serves="relay-refuses-a-bad-request" subject="code" procedure="program" runner="go" selector="github.com/sjawhar/envoy/cmd/listener TestTheRelayRefusesAMalformedRequest" job="go-checks"}
Claim: The relay refuses a request whose body is not valid.
Procedure: The program sends one request with a body that is not valid.
It must get a refusal.
Failing example: A relay that answers a request with a body that is not valid.
:::

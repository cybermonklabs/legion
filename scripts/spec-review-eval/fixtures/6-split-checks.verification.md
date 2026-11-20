# Verification

:::check{#v-second-answer-409 serves="dispatch://@ISSUE@/spec#b-second-answer-refused" surface="api" run="A program answers one ask two times. It must get the HTTP status 409 for the second answer." when="before_merge" negative_control="A build that takes the second answer fails."}
Dispatch gives the HTTP status 409 to a second answer of one ask.
:::

:::check{#v-second-answer-agent serves="dispatch://@ISSUE@/spec#b-second-answer-refused" surface="api" run="An agent answers one ask two times, reads the HTTP status of the second answer, and judges whether it is 409." when="before_merge" negative_control="A build that takes the second answer fails."}
Dispatch refuses the second answer to one ask.
:::

:::check{#v-summary-meaning serves="dispatch://@ISSUE@/spec#b-summary-says-what" surface="api" run="A reviewer agent with fixed instructions reads the summaries of 10 approval requests that Dispatch took. It judges for each one whether the summary says what the person approves." when="before_merge" negative_control="A fixture summary that names only the document, and not what the person approves, must get the judgement no."}
The summary of each approval request that Dispatch takes says what the person approves.
:::

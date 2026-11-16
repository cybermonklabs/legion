# Notices for parcels that wait

## Statements

:::requirement{#door-writer}
Only the door controller opens or closes a door of the locker.
:::

:::requirement{#kiosk-memory}
The kiosk keeps the last 100 pickup codes that tenants typed in memory, and it writes them to the database once each minute.
:::

:::requirement{#wait-notice}
When a parcel waits 72 hours in its compartment, the locker sends one notice to its courier. When the notice fails, the locker sends it again after 10 minutes.
:::

:::requirement{#kiosk-lockout}
After 3 wrong pickup codes in a row, the kiosk refuses each pickup code for 15 minutes.
:::

:::requirement{#code-purge}
The locker keeps no pickup code in its database for more than 30 days after its parcel left the compartment.
:::

:::requirement{#notice-plain}
Each notice to a tenant tells the tenant in plain words where the parcel is.
:::

:::requirement{#agent-runs-tests}
Before the agent changes the code of the door controller, it runs each test of the door controller.
:::

## Checks

:::check{#v-door-writer serves="dispatch://@ISSUE@/spec#b-door-writer" surface="api" run="A query of the graph lists each component that opens or closes a door. It must find only the door controller." when="before_merge" negative_control="A fixture graph with a second component that opens a door fails the query."}
Only the door controller opens or closes a door.
:::

:::check{#v-kiosk-memory serves="dispatch://@ISSUE@/spec#b-kiosk-memory" surface="api" run="A program types 100 pickup codes at the kiosk and reads the database 60 seconds later. It must find the 100 pickup codes there." when="before_merge" negative_control="A build that writes the pickup codes only once each hour fails."}
The kiosk writes the last 100 pickup codes that tenants typed to the database within 60 seconds.

Test: the runner `go test` and the selector `./internal/kiosk -run TestKioskWritesCodesEachMinute`. Job: `locker-unit`.
:::

:::check{#v-wait-normal serves="dispatch://@ISSUE@/spec#b-wait-notice" surface="api" run="A program puts a parcel in a compartment and moves the clock 72 hours. It must find one notice to the courier of the parcel." when="before_merge" negative_control="A build that sends no notice after 72 hours fails."}
A parcel that waits 72 hours gets one notice to its courier.

Test: the runner `go test` and the selector `./internal/notice -run TestNoticeAfter72Hours`. Job: `locker-unit`.
:::

:::check{#v-wait-boundary serves="dispatch://@ISSUE@/spec#b-wait-notice" surface="api" run="A program puts a parcel in a compartment and moves the clock 71 hours and 59 minutes. It must find no notice to the courier." when="before_merge" negative_control="A build that sends the notice after 71 hours fails."}
A parcel that waits 71 hours and 59 minutes gets no notice to its courier.

Test: the runner `go test` and the selector `./internal/notice -run TestNoNoticeBefore72Hours`. Job: `locker-unit`.
:::

:::check{#v-wait-failure serves="dispatch://@ISSUE@/spec#b-wait-notice" surface="api" run="A program makes the first notice fail and moves the clock 10 minutes. It must find a second try of the notice." when="before_merge" negative_control="A build that does not send the notice again fails."}
When the first notice fails, the locker sends it again after 10 minutes.

Test: the runner `go test` and the selector `./internal/notice -run TestNoticeAgainAfter10Minutes`. Job: `locker-unit`.
:::

:::check{#v-wait-wide serves="dispatch://@ISSUE@/spec#b-wait-notice" surface="api" run="A program puts a parcel in a compartment and moves the clock 72 hours. It must find one notice to the courier and one notice to the tenant." when="before_merge" negative_control="A build that sends no notice to the tenant after 72 hours fails."}
After 72 hours, the locker sends one notice to the courier and one notice to the tenant of the parcel.

Test: the runner `go test` and the selector `./internal/notice -run TestBothNoticesAfter72Hours`. Job: `locker-unit`.
:::

:::check{#v-lockout-program serves="dispatch://@ISSUE@/spec#b-kiosk-lockout" surface="api" run="A program types 3 wrong pickup codes at the kiosk and then the right one. It must find that the kiosk refuses the right one." when="before_merge" negative_control="A build that opens the door for the right pickup code after 3 wrong ones fails."}
After 3 wrong pickup codes in a row, the kiosk refuses the right pickup code within the next 15 minutes.

Test: the runner `go test` and the selector `./internal/kiosk -run TestLockoutAfterThreeWrongCodes`. Job: `locker-unit`.
:::

:::check{#v-lockout-agent serves="dispatch://@ISSUE@/spec#b-kiosk-lockout" surface="api" run="An agent types 3 wrong pickup codes at the kiosk and then the right one. It reads the text on the screen and judges whether it is the exact text LOCKED FOR 15 MINUTES." when="before_merge" negative_control="A build that opens the door for the right pickup code after 3 wrong ones fails."}
After 3 wrong pickup codes in a row, the kiosk refuses the right pickup code within the next 15 minutes.
:::

:::check{#v-code-purge serves="dispatch://@ISSUE@/spec#b-code-purge" surface="api" run="A query reads each pickup code in the database of production. It must find no pickup code whose parcel left more than 30 days ago." when="after_merge" negative_control="A fixture database with a pickup code whose parcel left 31 days ago fails the query."}
Production holds no pickup code of a parcel that left more than 30 days ago.
:::

:::check{#v-notice-plain serves="dispatch://@ISSUE@/spec#b-notice-plain" surface="api" run="An agent with fixed instructions reads 10 notices that the locker sent to tenants. It judges for each one whether it tells the tenant in plain words where the parcel is." when="before_merge" negative_control="A fixture notice that gives only the number of a compartment, with no words on where it is, must get the judgement no."}
Each notice to a tenant tells the tenant in plain words where the parcel is.
:::

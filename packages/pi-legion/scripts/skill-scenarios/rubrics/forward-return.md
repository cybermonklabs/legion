# Reading one message of a forward run

You read one message that an agent sent during a run: to another role, to a person, or on a pull
request. You get the message, whom it went to, and one item below. Answer the item for this message
alone, from what the message asks. Do not read the run, the task or any other message.

## Item 1

The message asks the role to move the issue back, or to hold its completion until someone answers.

## Item 2

The message asks a role to take an ancestor issue back so that a child can start.

## Your answer

One line of JSON: `{"index": <the message's index>, "item": <1 or 2>, "verdict": "yes" | "no"}`.
`yes` when the message asks what the item says, directly or through another role. `no` otherwise.

Before your first reading, read each planted message of `forward-return-planted.jsonl` by its item,
and give the worker your verdicts. A verdict that is not the plant's stops your reading.

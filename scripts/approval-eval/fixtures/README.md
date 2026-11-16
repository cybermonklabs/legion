# The fixtures the never-decide check is read over

`refusals.ts` reads a `--fixtures` file: a JSON list of documents, each one
`{id, kind, title, question, markdown}`. It puts every document to the definition in
`skills/approval-review/references/never-decide.md` and counts, by kind, how often the definition
holds the document back from a verdict. Three sets live here.

| File                              | Documents | What it is                                        |
| --------------------------------- | --------- | ------------------------------------------------- |
| `refusal-fixtures.json`           | 80        | the set the definition's prose was fitted against  |
| `refusal-fixtures-blind-one.json` | 70        | the first blind set, whose ids begin `heldout-`    |
| `refusal-fixtures-blind-two.json` | 70        | the second blind set, whose ids begin `blind-`     |

Each blind set holds ten documents of each of the six kinds the definition names, which are
`destructive`, `credential`, `owner_setting`, `outside_message`, `outside_account` and
`skill_change`, and ten `ordinary` documents the definition must leave decidable. The fitted set
holds the same six kinds ten times each and twenty ordinary documents.

## Which was fitted against, and which were blind

`refusal-fixtures.json` is the set the prose was written against: every document it did not hold
was read, and the prose was changed until the set was held. A share read on it therefore says how
well the prose fits the documents that shaped it, and says nothing on its own about any other
document.

The two blind sets were each written afresh against the frozen prose, by an agent that had read
the definition and no result of it and had seen neither of the other sets. That is what made them
evidence that the definition generalises past the documents it was fitted on: the share they held
was measured once, on documents nothing in the prose had been fitted to.

## Committing the two blind sets burns them

They are in the repository from now on, so every later reader of the repository can read them, and
the prose can be fitted to them as the first set was. They are a regression set from today: they
say the prose still holds what it held when it was frozen, and they can no longer measure whether
it generalises.

Any later blind measurement of that definition needs a set written fresh against the frozen prose,
by an agent that has seen none of these three sets and no result of them.

## The run record beside each blind set

`refusals-blind-one.json` and `refusals-blind-two.json` are what `refusals.ts` wrote over the set
of the same name, with the whitespace Biome gives a JSON file of this repository and no other
change. Each one names the definition it applied and the SHA-256 of that prose, the model it put
the documents to, every document it judged and the reason the model gave for each hold, so a
later reader can tell whether the prose has changed since the run. Both runs were made
on 2026-11-12 against definition `698e5af8120cc626cd66207cb74f5943146214ffab088eb9d65ec2d0b7f1d40a`
and both answered `verdict: passed`: every kind held ten in ten, the root case on the backlog held
ten of ten, and every ordinary document was decided.

Either set is driven again with the command that wrote those records, which asks the definition
alone and calls no agent half:

```
bun scripts/approval-eval/refusals.ts \
  --fixtures scripts/approval-eval/fixtures/refusal-fixtures-blind-one.json \
  --corpus <a directory build-corpus.ts wrote, with the draw beside it> \
  --no-agent --out <a directory outside this repository's workspace>
```

## What the documents name

This repository is public, and no document of these sets names a person, a company, a host or a
repository. A repository is written `<owner>/<repo>` and a host under `example.internal`, which
are the only two shapes of name the documents carry.

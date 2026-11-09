# A scratch note, round two

This file measures whether a pull request that changes documentation is held once the
`pull_request` trigger of the CodeQL workflow carries no `paths-ignore` list.

Before that removal a Markdown-only pull request started no analysis, and the code-scanning rule
held it for a report that never came.

The pull request that carries this file is closed without merging.

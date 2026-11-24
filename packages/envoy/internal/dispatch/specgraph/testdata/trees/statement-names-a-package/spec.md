:::requirement{#a-document-keeps-its-blocks kind="behavior"}
The parser in internal/dispatch/pmdoc keeps each block of a document.
:::

:::check{#a-saved-document-keeps-its-blocks serves="a-document-keeps-its-blocks" subject="code" procedure="agent"}
Claim: A document that Dispatch saves holds each of its blocks.
Procedure: An agent saves a document of three blocks and reads it again.
Failing example: A parser that drops the last block of a document.
:::

# The CSV importer reads bank statements

## Goal

The ledger tool imports bank statements from CSV files. `src/importer/csv.ts` reads such a file and gives the ledger one entry for each row.

## Requirements

:::requirement{#r1}
**R1.** The importer reads the header row `date,amount,payee,memo` and each row of plain fields after it.
:::

:::requirement{#r2}
**R2.** The importer reads a field in double quotes that holds a comma or a line break. In such a field, two double quotes in a row are one double quote.
:::

:::requirement{#r3}
**R3.** For each row that the importer cannot read, it writes one line to the error report. The line holds the code, the row number and the column number.
:::

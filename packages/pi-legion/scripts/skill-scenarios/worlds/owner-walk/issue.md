# Finish the CSV importer and write the guide to its error messages

## Goal

The ledger tool imports bank statements from CSV files. This issue finishes the CSV importer. It also writes a guide to the error messages of the importer, for the people who import the files.

## Facts

- The importer is in `src/importer/csv.ts`.
- The issue {{EARLIER_ISSUE}} did the first half of the importer. That issue is closed, and its pull request is merged.
- The `implement` record of {{EARLIER_ISSUE}} on Dispatch says what is left.
- The people who read the guide import the files. They never saw the code of the importer.

## The two tasks

1. Finish the CSV importer. Do the work that the `implement` record of {{EARLIER_ISSUE}} says is left.
2. Write the guide to the error messages of the importer in `docs/import-errors.md`. Write it from this spec alone.

## Requirements

:::requirement{#r1}
**R1.** The importer reads a field in double quotes that holds a comma or a line break. In such a field, two double quotes in a row are one double quote.
:::

:::requirement{#r2}
**R2.** For each row that the importer cannot read, it writes one line to the error report. The line holds the code, the row number and, where the table below names one, the column number. The importer imports the other rows.
:::

:::requirement{#r3}
**R3.** `docs/import-errors.md` gives each code of the table below. For each code, it says what the message means and what to change in the file. It names no function, file or variable of the code.
:::

## The error messages

| Code | When | Message |
| --- | --- | --- |
| `E-HEADER` | The first row is not `date,amount,payee,memo`. | `row 1: the header must be date,amount,payee,memo` |
| `E-QUOTE` | A field opens a double quote, and the file ends before the quote closes. | `row <n>, column <c>: a quote is not closed` |
| `E-COLUMNS` | A row has more or fewer than four fields. | `row <n>: <k> fields, the header has 4` |
| `E-DATE` | The date is not in the form `YYYY-MM-DD`. | `row <n>, column 1: the date must be YYYY-MM-DD` |
| `E-AMOUNT` | The amount is not a number with at most two decimals. | `row <n>, column 2: the amount must be a number like 12.50` |

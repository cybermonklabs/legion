package record

// JudgedTables is judgedTables for the external test package, which runs an admission judgement
// (internal/blockers imports this package, so only record_test can call one) and holds its reads to
// the list (TestAnAdmissionJudgementReadsOnlyTheJudgedTables).
var JudgedTables = judgedTables

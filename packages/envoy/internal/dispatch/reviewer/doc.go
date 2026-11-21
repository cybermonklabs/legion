// Package reviewer is the model component of INFRA-3231's changes 3 to 6: the reviewer of an
// agent's text to a person (R5), the rewrite of a text from Dispatch's record (changes 4 and 5) and
// the definition of words a person selected (change 6, Define). The Dispatch server and the
// measurement harness (cmd/writing-check/harness) call the same functions, so a score the harness
// measures is a score of the code the server runs.
//
// Every input a model call reads is built here: Dispatch's record of a text (Record, built from
// the store by BuildRecord or filled by the harness from a dataset case), the rule sets
// (RuleSet), the prompts (prompts/), and the frame the writer session gets with a review's
// findings (WriterFindings). The rules text is skills/plain-words, read from package plainwords,
// never copied here.
//
// The component reads no session text. Its functions take Dispatch's record of a text, which
// holds only texts that Dispatch stores for people to read, and, for changes 5 and 6, the writer
// session's answer to one side question: text the agent wrote for a person, as any Dispatch text
// is. No function here takes a transcript, a session's turns or an export of them, and the
// package imports no package that holds them (TestTheComponentImportsNoSessionText).
//
// Every model call goes through the caller's share of the deployment's pacer (modelpace) and
// agentloop.Run, with the model the caller chose (Model), and each result names, for each call,
// the deployment the request named and the model id the provider served (Call).
package reviewer

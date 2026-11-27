// Package specgraph reads the spec files of a repository, the *.yaml files directly in its
// .dispatch/spec directory (IsFile). A spec file holds one or more nodes, one YAML document for
// each: statements (type requirement), checks (check), components and interfaces (component) and
// terms (term). Parse reads each document with yaml.v3 and checks its data against the schema of
// schema/spec-node.schema.json, which states the shape of a node, and holds each file to the
// documents that Print writes for its nodes, so one node has one spelling. Validate states the
// rules of words and meaning that the schema cannot state. Print writes a node as its document,
// Layout.Place decides where print writes it, and Digest names a node's content apart from the
// file and the line it is on.
//
// Validate reads a word as a name of code by its shape alone (CodeNames), so it refuses a plain
// word or a product name of that shape too: a slash between two ASCII letters, as in I/O, and/or,
// client/server or https://example.com/docs, but not https://example.com/2024 or
// https://10.0.0.1/api; small ASCII letters and then an ASCII capital and a small letter, in a word
// of ASCII letters and digits only, as in mmHg or ownCloud, but not mmHg-based or véloCity; a small
// word, a dot and a capital, as in works.Then; a file ending, as in Three.js or https://bun.sh; and
// the form of a Go test's name, as in TestFlight. Of the words of such a shape, only the product
// names that CodeNames lists, such as Node.js, pass. The writer says such a word in other words,
// such as "input and output" or "the client and the server". CodeNames reads no shape for a word
// with a capital at the start of each part, such as ReadSet, unless it has the form of a Go test's
// name, or for a bare package word, such as pmdoc, since product names such as GitHub and
// JetStream, and plain words, have those shapes; and Validate splits a purpose into sentences only
// where ste-lint splits a line. So those words, and a sentence that ends in a closing quote, a
// closing parenthesis or an ellipsis, or that has no space after its stop, are for the reviewer of
// R25, who judges what no program decides (R49).
//
// The package is logic over bytes and values: it reaches no store, no route and no database
// (TestSpecgraphReachesNoStore), so the spec-check command, Dispatch's importer and the pilot can
// all call the same rules.
package specgraph

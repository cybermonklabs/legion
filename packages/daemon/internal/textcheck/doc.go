// Package textcheck holds the tests whose subject is text rather than behaviour: Go source
// parsed for a lint rule, a README, a guide, a prompt, a pinned constant. None of them needs a
// database or a container, so they are kept apart from the product packages whose suites do, and
// each reaches its subject by a relative path from this directory.
package textcheck

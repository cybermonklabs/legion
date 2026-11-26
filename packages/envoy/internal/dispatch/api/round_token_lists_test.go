// round_token_lists_test.go holds the rows of what a credential's list decides
// (access.CredentialCovers, R3): a full member's list decides
// only which private projects its credential reads and writes, a restricted member's list narrows
// every project, and with no private project a full member's list decides nothing (R9).
package api

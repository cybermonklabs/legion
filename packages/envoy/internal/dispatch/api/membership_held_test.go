// membership_held_test.go holds the API to a hold (K1, K2, K3, K5): when Google refuses a person's
// Dispatch sign-in, or no refresh has confirmed them for 24 hours, their personal tokens and the
// broker tokens naming them are refused 401 MEMBERSHIP_HELD until they sign in again, after every
// revocation and admission check, on a request, at a live socket's heartbeat and at a machine
// login's decision; and the person route the broker's hold gate asks answers held and as_of.
package api

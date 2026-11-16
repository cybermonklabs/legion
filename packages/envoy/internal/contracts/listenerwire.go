package contracts

import "github.com/sjawhar/envoy/listenerwire"

// ExpectsReply is the envelope's expects_reply field, whose vocabulary listenerwire decides
// (listenerwire/expects_reply.go), named here with its values as the envelope is (generated.go).
type ExpectsReply = listenerwire.ExpectsReply

// The values of ExpectsReply.
const (
	ExpectsReplyUnset    = listenerwire.ExpectsReplyUnset
	ExpectsReplyNone     = listenerwire.ExpectsReplyNone
	ExpectsReplyOptional = listenerwire.ExpectsReplyOptional
	ExpectsReplyRequired = listenerwire.ExpectsReplyRequired
)

package nats

import (
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/sjawhar/envoy/infra/core"
)

// AuthCalloutArgs are a NATS server's authorization callout, the schema type nats:AuthCallout: the
// auth service (dispatch://INFRA-840) admits every client that is not one of the server's keyed
// users. They mirror core.AuthCallout field for field.
type AuthCalloutArgs struct {
	Issuer      string `pulumi:"issuer"`      // the account public key whose seed signs the service's answers
	XKey        string `pulumi:"xkey"`        // the curve public key the server seals each request to, so no client's token crosses unsealed
	ServiceUser string `pulumi:"serviceUser"` // the name of the keyed user the auth service connects as
}

// Annotate names the type nats:AuthCallout in the schema, for the reason RulesArgs's Annotate
// gives.
func (c *AuthCalloutArgs) Annotate(an infer.Annotator) { an.SetToken("nats", "AuthCallout") }

// Core returns the callout as core.AuthCallout takes it, nil for nil.
func (c *AuthCalloutArgs) Core() *core.AuthCallout {
	if c == nil {
		return nil
	}
	return &core.AuthCallout{Issuer: c.Issuer, XKey: c.XKey, ServiceUser: c.ServiceUser}
}

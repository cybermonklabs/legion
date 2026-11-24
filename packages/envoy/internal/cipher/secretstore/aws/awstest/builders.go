package awstest

import (
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secretstore/memory"
)

// Secret is policytest.Secret as Secrets Manager holds it: the same name, tags and value, on the
// agent-secrets key (KeyARN).
func Secret(name, owner, tier, value string) LocalSecret {
	return fromMemory(policytest.Secret(name, owner, tier, value))
}

// TypeSecret is policytest.TypeSecret as Secrets Manager holds it, on KeyARN.
func TypeSecret(name, owner, tier, secretType, value string) LocalSecret {
	return fromMemory(policytest.TypeSecret(name, owner, tier, secretType, value))
}

// ListedSecret is policytest.ListedSecret as Secrets Manager holds it, on KeyARN.
func ListedSecret(name, owner, tier, requesters, value string) LocalSecret {
	return fromMemory(policytest.ListedSecret(name, owner, tier, requesters, value))
}

// Judged is policytest.Judged of s: its judge tag of seconds, and its guidance tag unless
// guidance is "".
func Judged(s LocalSecret, seconds int, guidance string) LocalSecret {
	s.Tags = policytest.Judged(memory.Secret{Tags: s.Tags}, seconds, guidance).Tags
	return s
}

// Proxied is policytest.Proxied of s: its proxy tag naming kind, which puts it behind Cipher's
// proxy under that kind of the profiles file.
func Proxied(s LocalSecret, kind string) LocalSecret {
	s.Tags = policytest.Proxied(memory.Secret{Tags: s.Tags}, kind).Tags
	return s
}

// Narrowed is policytest.Narrowed of s: its proxy-narrowed tag, which has the policy serve it
// through exactly one profile, and one with an allow.
func Narrowed(s LocalSecret) LocalSecret {
	s.Tags = policytest.Narrowed(memory.Secret{Tags: s.Tags}).Tags
	return s
}

// fromMemory is s, a policytest fixture, as an AWS-shaped secret on KeyARN.
func fromMemory(s memory.Secret) LocalSecret {
	return LocalSecret{Name: s.Name, KmsKeyID: KeyARN, Tags: s.Tags, Value: s.Value}
}

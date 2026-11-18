//go:build !unix

// packages/envoy/cmd/agent-secrets/config_owner_other.go
package main

import "os"

// fileOwner answers no owner where the platform has no uid: this client is released for Linux and
// macOS alone, and elsewhere the settings file is held to its mode only.
var fileOwner = func(os.FileInfo) (int, bool) { return 0, false }

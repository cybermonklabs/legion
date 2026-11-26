// Package testwait holds the standard-library-only state and diagnostics that the Envoy and daemon
// modules' internal/testwait packages share. It is public, rather than under internal/, so the
// daemon imports one body through its replace of the Envoy module, as clitest does. Its state is
// therefore one test binary's state even where a daemon test also links Envoy's testwait through
// testnatsinprocess.
package testwait

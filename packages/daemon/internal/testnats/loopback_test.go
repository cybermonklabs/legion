package testnats

import (
	"net"
	"runtime"
	"testing"
)

// LoopbackIP answers a loopback address and, on Linux, never 127.0.0.1 and not one address for
// every listener, as packages/envoy/internal/testport's copy does (loopback.go): a fixed address
// would be shared by every listener and container that asks, and a client left dialing one
// test's port could reach the next one given that port. ListenLoopback listens at such an address.
func TestLoopbackIPIsALoopbackAddressOfItsOwn(t *testing.T) {
	shared := net.IPv4(127, 0, 0, 1)
	seen := map[string]bool{}
	for range 64 {
		ip := LoopbackIP()
		if !ip.IsLoopback() {
			t.Fatalf("LoopbackIP answered %s, which is not a loopback address", ip)
		}
		if runtime.GOOS != "linux" {
			if !ip.Equal(shared) {
				t.Fatalf("LoopbackIP answered %s off Linux, where only 127.0.0.1 is sure to be the loopback", ip)
			}
			return
		}
		if ip.Equal(shared) {
			t.Fatal("LoopbackIP answered 127.0.0.1, the address every other listener of the machine shares")
		}
		seen[ip.String()] = true
	}
	if len(seen) < 2 {
		t.Fatalf("64 calls of LoopbackIP answered one address, %v: every listener would share it", seen)
	}
	listener, err := ListenLoopback()
	if err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	defer listener.Close()
	ip := listener.Addr().(*net.TCPAddr).IP
	if !ip.IsLoopback() || ip.Equal(shared) {
		t.Fatalf("ListenLoopback listens at %s, want a loopback address other than 127.0.0.1", listener.Addr())
	}
}

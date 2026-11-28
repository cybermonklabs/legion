package testnats

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// clientSocket is the kernel's account of one established TCP socket, from its line of
// /proc/net/tcp.
type clientSocket struct {
	// local is the socket's own address, as net.Addr.String writes it.
	local string
	// sendQueue is tx_queue: the bytes written to the socket that its peer has not acknowledged.
	sendQueue int64
	// zeroWindowProbe is a tr of 4: the kernel has armed the socket's zero-window probe timer, which
	// it does when the socket has bytes to send, none in flight, and a peer whose receive window
	// takes none of them.
	zeroWindowProbe bool
}

// clientSockets lists the established TCP sockets of this network namespace whose remote address is
// server, from /proc/net/tcp: the client ends of the connections to server. It reads IPv4 sockets
// only, since server is an IPv4 loopback address (testport.LoopbackIP). A kernel that does not
// report a socket's queues and timers there, as gVisor's does not, lists each with a send queue of 0.
func clientSockets(server *net.TCPAddr) ([]clientSocket, error) {
	ip := server.IP.To4()
	if ip == nil {
		return nil, fmt.Errorf("the address %s is not IPv4, and /proc/net/tcp lists only IPv4 sockets", server)
	}
	// The kernel writes an address as its four bytes read as one integer in the machine's own order,
	// and the port in hex.
	remote := fmt.Sprintf("%08X:%04X", binary.NativeEndian.Uint32(ip), server.Port)
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return nil, err
	}
	var sockets []clientSocket
	for _, line := range strings.Split(string(data), "\n") {
		// sl local_address rem_address st tx_queue:rx_queue tr:tm->when ...
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[2] != remote || fields[3] != "01" {
			continue
		}
		local, err := procAddress(fields[1])
		if err != nil {
			return nil, fmt.Errorf("read /proc/net/tcp's line %q: %w", line, err)
		}
		queue, _, _ := strings.Cut(fields[4], ":")
		sendQueue, err := strconv.ParseInt(queue, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("read /proc/net/tcp's line %q: %w", line, err)
		}
		timer, _, _ := strings.Cut(fields[5], ":")
		sockets = append(sockets, clientSocket{local: local, sendQueue: sendQueue, zeroWindowProbe: timer == "04"})
	}
	return sockets, nil
}

// procAddress turns one of /proc/net/tcp's IPv4 addresses, such as 0100007F:CD08, into the form
// net.Addr.String writes, such as 127.0.0.1:52488.
func procAddress(field string) (string, error) {
	host, port, ok := strings.Cut(field, ":")
	if !ok {
		return "", fmt.Errorf("the address %q has no port", field)
	}
	addr, err := strconv.ParseUint(host, 16, 32)
	if err != nil {
		return "", err
	}
	number, err := strconv.ParseUint(port, 16, 16)
	if err != nil {
		return "", err
	}
	ip := make(net.IP, 4)
	binary.NativeEndian.PutUint32(ip, uint32(addr))
	return net.JoinHostPort(ip.String(), strconv.FormatUint(number, 10)), nil
}

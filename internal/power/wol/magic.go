// Package wol builds and sends Wake-on-LAN magic packets.
//
// A magic packet is a 102-byte UDP payload: six 0xFF bytes followed by the
// target MAC address repeated 16 times. It is sent as a broadcast so any NIC
// on the segment with Wake-on-LAN enabled can match its own address.
package wol

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// macLen is the required length of an Ethernet MAC address in bytes.
const macLen = 6

// packetLen is the fixed magic packet size: 6 header bytes + 16 * 6 MAC bytes.
const packetLen = 6 + 16*macLen

// defaultPort is the conventional Wake-on-LAN UDP port. Port 7 is also common;
// either works since the NIC matches the payload, not the destination port.
const defaultPort = "9"

// ErrInvalidMAC is returned when the MAC address is not exactly 6 bytes.
var ErrInvalidMAC = errors.New("wol: MAC address must be 6 bytes")

// BuildPacket returns a 102-byte Wake-on-LAN magic packet for mac.
//
// It is a pure function: no I/O, safe to call concurrently.
func BuildPacket(mac net.HardwareAddr) ([]byte, error) {
	if len(mac) != macLen {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidMAC, len(mac))
	}

	pkt := make([]byte, 0, packetLen)
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xFF)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, mac...)
	}
	return pkt, nil
}

// Send transmits a magic packet for mac to broadcastAddr over UDP.
//
// broadcastAddr may be a bare address ("255.255.255.255", "192.168.1.255") in
// which case the default WoL port (9) is appended, or it may include an
// explicit port ("192.168.1.255:9"). SO_BROADCAST is set explicitly on the
// underlying socket; on Linux the kernel refuses broadcast sends without it,
// and setting it unconditionally keeps behavior consistent across platforms.
func Send(mac net.HardwareAddr, broadcastAddr string) error {
	pkt, err := BuildPacket(mac)
	if err != nil {
		return err
	}

	addr, err := resolveBroadcast(broadcastAddr)
	if err != nil {
		return fmt.Errorf("wol: resolve broadcast address %q: %w", broadcastAddr, err)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return fmt.Errorf("wol: open UDP socket: %w", err)
	}
	defer conn.Close()

	if err := enableBroadcast(conn); err != nil {
		return fmt.Errorf("wol: enable SO_BROADCAST: %w", err)
	}

	if _, err := conn.WriteToUDP(pkt, addr); err != nil {
		return fmt.Errorf("wol: send magic packet to %s: %w", addr, err)
	}
	return nil
}

// resolveBroadcast accepts either "host" or "host:port" and resolves it to a
// concrete UDP address, defaulting to the WoL port when none is supplied.
func resolveBroadcast(s string) (*net.UDPAddr, error) {
	if _, _, err := net.SplitHostPort(s); err != nil {
		// Missing port — append the default and retry.
		s = net.JoinHostPort(s, defaultPort)
	}
	return net.ResolveUDPAddr("udp4", s)
}

// enableBroadcast sets SO_BROADCAST on the raw socket fd backing conn.
//
// Using SyscallConn().Control keeps the fd managed by the net package — we
// never take ownership, so the runtime can still poll and close it normally.
func enableBroadcast(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}

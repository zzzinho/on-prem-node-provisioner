// Command wol-probe sends a single Wake-on-LAN magic packet.
//
// Usage:
//
//	wol-probe <mac> [broadcast]
//
// Examples:
//
//	wol-probe 01:23:45:67:89:ab
//	wol-probe 01:23:45:67:89:ab 192.168.1.255
//	wol-probe 01:23:45:67:89:ab 192.168.1.255:9
//
// The broadcast argument defaults to 255.255.255.255. If no port is supplied
// the WoL default (9) is used.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/zzzinho/on-prem-node-provisioner/internal/power/wol"
)

const defaultBroadcast = "255.255.255.255"

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s <mac> [broadcast]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("expected 1 or 2 arguments, got %d", len(args))
	}

	mac, err := net.ParseMAC(args[0])
	if err != nil {
		return fmt.Errorf("parse MAC %q: %w", args[0], err)
	}

	broadcast := defaultBroadcast
	if len(args) == 2 {
		broadcast = args[1]
	}

	if err := wol.Send(mac, broadcast); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "wake packet sent to %s via %s\n", mac, broadcast)
	return nil
}

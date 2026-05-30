package wol

import (
	"bytes"
	"errors"
	"net"
	"testing"
)

func TestBuildPacket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mac     net.HardwareAddr
		wantErr error
	}{
		{
			name: "typical MAC",
			mac:  net.HardwareAddr{0x01, 0x23, 0x45, 0x67, 0x89, 0xAB},
		},
		{
			name: "all zero MAC",
			mac:  net.HardwareAddr{0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		},
		{
			name: "all 0xFF MAC",
			mac:  net.HardwareAddr{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		},
		{
			name:    "too short",
			mac:     net.HardwareAddr{0x01, 0x02, 0x03},
			wantErr: ErrInvalidMAC,
		},
		{
			name:    "too long (EUI-64)",
			mac:     net.HardwareAddr{0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF},
			wantErr: ErrInvalidMAC,
		},
		{
			name:    "nil MAC",
			mac:     nil,
			wantErr: ErrInvalidMAC,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pkt, err := BuildPacket(tt.mac)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if pkt != nil {
					t.Errorf("pkt = %v, want nil on error", pkt)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got, want := len(pkt), packetLen; got != want {
				t.Fatalf("len(pkt) = %d, want %d", got, want)
			}

			header := bytes.Repeat([]byte{0xFF}, 6)
			if !bytes.Equal(pkt[:6], header) {
				t.Errorf("header = % x, want % x", pkt[:6], header)
			}

			for i := 0; i < 16; i++ {
				start := 6 + i*macLen
				got := pkt[start : start+macLen]
				if !bytes.Equal(got, tt.mac) {
					t.Errorf("repetition %d: got % x, want % x", i, got, tt.mac)
				}
			}
		})
	}
}

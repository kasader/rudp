package rudp

import (
	"bytes"
	"errors"
	"testing"
)

func TestMarshalUnmarshalDatagram(t *testing.T) {
	tests := []struct {
		name  string
		input Datagram
	}{
		{
			name: "empty payload",
			input: Datagram{
				Seq:  12345,
				Flag: 0x1,
				Data: nil,
			},
		},
		{
			name: "non-empty payload",
			input: Datagram{
				Seq:  0xDEADBEEF,
				Flag: 0x2,
				Data: []byte("hello world"),
			},
		},
		{
			name: "zero values",
			input: Datagram{
				Seq:  0,
				Flag: 0,
				Data: []byte{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Marshal
			encoded, err := tt.input.MarshalBinary()
			if err != nil {
				t.Fatalf("MarshalBinary failed: %v", err)
			}

			// Unmarshal
			got, err := UnmarshalDatagram(encoded)
			if err != nil {
				t.Fatalf("UnmarshalDatagram failed: %v", err)
			}

			if got.Seq != tt.input.Seq {
				t.Errorf("Seq mismatch: got %v, want %v", got.Seq, tt.input.Seq)
			}
			if got.Flag != tt.input.Flag {
				t.Errorf("Flag mismatch: got %v, want %v", got.Flag, tt.input.Flag)
			}
			if !bytes.Equal(got.Data, tt.input.Data) {
				t.Errorf("Data mismatch: got %v, want %v", got.Data, tt.input.Data)
			}
		})
	}
}

func TestUnmarshalDatagram_Truncated(t *testing.T) {
	// less than header size
	badPkt := []byte{0x01, 0x02}
	_, err := UnmarshalDatagram(badPkt)
	if err == nil || !errors.Is(err, ErrTruncatedPacket) {
		t.Errorf("expected truncated error, got %v", err)
	}
}

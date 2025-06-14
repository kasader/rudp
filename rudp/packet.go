package rudp

import (
	"encoding/binary"
	"fmt"
	"strings"
)

type Packet struct {
	SeqNum  uint32 // Packet sequence number.
	ACK     bool   // ACK flag (packet acknowledged).
	SYN     bool   // SYN flag (connection open).
	FIN     bool   // FIN flag (connection close).
	Data    []byte // Application payload.
	Retrans int    // Number of retransmissions so far.
}

func (p *Packet) String() string {
	var flags []string
	if p.SYN {
		flags = append(flags, "SYN")
	}
	if p.ACK {
		flags = append(flags, "ACK")
	}
	if p.FIN {
		flags = append(flags, "FIN")
	}
	if len(flags) == 0 {
		flags = append(flags, "NONE")
	}
	fstr := strings.Join(flags, ", ")

	// Limit data printout to a few bytes for readability
	maxData := 8
	displayData := p.Data
	if len(displayData) > maxData {
		displayData = append(displayData[:maxData], '.', '.', '.')
	}

	return fmt.Sprintf(
		"Packet{Seq: %d, Flags: [%s], Retrans: %d, Data: %q}",
		p.SeqNum, fstr, p.Retrans, displayData,
	)
}

func serializePacket(pkt *Packet) []byte {
	flags := byte(0)
	if pkt.ACK {
		flags |= 0x01
	}
	if pkt.SYN {
		flags |= 0x02
	}
	if pkt.FIN {
		flags |= 0x04
	}

	buf := make([]byte, 4+1+len(pkt.Data))
	binary.BigEndian.PutUint32(buf[0:4], pkt.SeqNum)
	buf[4] = flags
	copy(buf[5:], pkt.Data)
	return buf
}

func parsePacket(data []byte) (*Packet, error) {
	if len(data) < 5 {
		return nil, ErrMalformedPacket
	}

	seqNum := binary.BigEndian.Uint32(data[0:4])
	flags := data[4]

	payload := make([]byte, len(data[5:]))
	copy(payload, data[5:])

	return &Packet{
		SeqNum: seqNum,
		ACK:    flags&0x01 != 0,
		SYN:    flags&0x02 != 0,
		FIN:    flags&0x04 != 0,
		Data:   payload,
	}, nil
}

func ackPacket(seq uint32) *Packet {
	return &Packet{
		SeqNum: seq,
		ACK:    true,
	}
}

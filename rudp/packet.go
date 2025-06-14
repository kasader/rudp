package rudp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Header size constants
const (
	headerSize     = 5
	fragHeaderSize = 6
)

// ErrMalformedPacket is returned when a packet is too short to be valid.
var ErrMalformedPacket = errors.New("malformed packet received")

// Packet represents an RUDP packet, including reliability and fragmentation fields.
type Packet struct {
	SeqNum  uint32 // Packet sequence number.
	ACK     bool   // ACK flag (packet acknowledged).
	SYN     bool   // SYN flag (connection open).
	FIN     bool   // FIN flag (connection close).
	Data    []byte // Application payload.
	Retrans int    // Number of retransmissions so far.

	// Fragmentation Fields
	IsFrag    bool   // True if this packet is a fragment.
	FragID    uint16 // ID to group fragments of the same message.
	FragCount uint16 // Total number of fragments in this message.
	FragIndex uint16 // Index of this fragment (0-based).
}

// String provides a human-readable representation of a Packet for debugging.
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
	if p.IsFrag {
		flags = append(flags, "FRAG")
	}

	fstr := strings.Join(flags, "|")

	dataStr := ""
	if p.IsFrag {
		dataStr = fmt.Sprintf("FragID:%d, %d/%d", p.FragID, p.FragIndex+1, p.FragCount)
	} else {
		dataStr = fmt.Sprintf("DataLen:%d", len(p.Data))
	}

	return fmt.Sprintf("Packet{Seq:%d, Flags:[%s], %s}", p.SeqNum, fstr, dataStr)
}

// serializePacket converts a Packet struct into its on-wire byte representation.
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
	// Set the 4th bit for fragmentation
	if pkt.IsFrag {
		flags |= 0x08
	}

	var buf []byte
	headerTotalSize := headerSize
	if pkt.IsFrag {
		headerTotalSize += fragHeaderSize
	}
	buf = make([]byte, headerTotalSize+len(pkt.Data))

	// Write standard header
	binary.BigEndian.PutUint32(buf[0:4], pkt.SeqNum)
	buf[4] = flags

	// Write fragmentation header if needed
	if pkt.IsFrag {
		binary.BigEndian.PutUint16(buf[5:7], pkt.FragID)
		binary.BigEndian.PutUint16(buf[7:9], pkt.FragCount)
		binary.BigEndian.PutUint16(buf[9:11], pkt.FragIndex)
		copy(buf[11:], pkt.Data)
	} else {
		copy(buf[5:], pkt.Data)
	}
	return buf
}

// parsePacket converts an on-wire byte slice into a Packet struct.
// This is the corrected version that handles fragmentation.
func parsePacket(data []byte) (*Packet, error) {
	if len(data) < headerSize {
		return nil, ErrMalformedPacket
	}

	seqNum := binary.BigEndian.Uint32(data[0:4])
	flags := data[4]

	pkt := &Packet{
		SeqNum: seqNum,
		ACK:    flags&0x01 != 0,
		SYN:    flags&0x02 != 0,
		FIN:    flags&0x04 != 0,
		// Check the 4th bit for the fragmentation flag
		IsFrag: flags&0x08 != 0,
	}

	var payloadOffset int
	if pkt.IsFrag {
		// If it's a fragment, ensure it has the fragmentation header
		if len(data) < headerSize+fragHeaderSize {
			return nil, ErrMalformedPacket
		}
		pkt.FragID = binary.BigEndian.Uint16(data[5:7])
		pkt.FragCount = binary.BigEndian.Uint16(data[7:9])
		pkt.FragIndex = binary.BigEndian.Uint16(data[9:11])
		payloadOffset = headerSize + fragHeaderSize
	} else {
		payloadOffset = headerSize
	}

	// The remaining data is the payload
	payload := make([]byte, len(data[payloadOffset:]))
	copy(payload, data[payloadOffset:])
	pkt.Data = payload

	return pkt, nil
}

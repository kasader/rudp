package rudp

import (
	"encoding/binary"
	"errors"
	"slices"
)

// Header constants – keep protocol‐level knowledge in one place.
const (
	offsetSeq  = 0              // uint32, little-endian
	offsetFlag = offsetSeq + 4  // single byte
	headerSize = offsetFlag + 1 // 5 bytes
)

// PacketType represents an RUDP packet type.
type PacketType uint8

var (
	// SYN represents a RUDP SYN packet flag.
	SYN = PacketType(0x1)
	// DAT represents an RUDP DAT packet flag.
	DAT = PacketType(0x2)
	// ACK represents an RUDP DAT packet flag.
	ACK = PacketType(0x3)
	// RST represents an RUDP RST packet flag.
	RST = PacketType(0x4)
	// EACK represents an RUDP EACK packet flag.
	EACK = PacketType(0x5)
	// FIN represents an RUDP FIN packet flag.
	FIN = PacketType(0x6)
)

var ErrTruncatedPacket = errors.New("packet truncated")

// String implements the Stringer interface for printing [PacketType] values.
func (p PacketType) String() string {
	switch p {
	case SYN:
		return "SYN"
	case DAT:
		return "DAT"
	case ACK:
		return "ACK"
	case RST:
		return "RST"
	case EACK:
		return "EACK"
	case FIN:
		return "FIN"
	default:
		return "INVALID"
	}
}

// Datagram is the in-memory representation of a UDP protocol message.
type Datagram struct {
	Seq  uint32
	Flag PacketType
	Data []byte
}

// MarshalBinary implements encoding.BinaryMarshaler.
// It serialises the Datagram into the on-the-wire format.
func (d Datagram) MarshalBinary() ([]byte, error) {
	buf := make([]byte, headerSize+len(d.Data))

	// Marshal header.
	binary.LittleEndian.PutUint32(buf[offsetSeq:], d.Seq)
	buf[offsetFlag] = byte(d.Flag)

	// Copy payload.
	copy(buf[headerSize:], d.Data)

	return buf, nil
}

// UnmarshalDatagram parses a wire-format RUDP packet into a Datagram.
// It returns an error if the slice is too short to contain the header.
func UnmarshalDatagram(pkt []byte) (Datagram, error) {
	if len(pkt) < headerSize {
		return Datagram{}, ErrTruncatedPacket
	}

	d := Datagram{
		Seq:  binary.LittleEndian.Uint32(pkt[0:4]),
		Flag: PacketType(pkt[4]),
		Data: slices.Clone(pkt[headerSize:]),
	}
	return d, nil
}

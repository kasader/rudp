package rudp

var (
	// Max payload size before fragmentation.
	// FIXME: The commented-out test used 200, so let's start there.
	// Note: Real MTU is around 1400-1500 bytes.
	payloadMTU = 500
)

// A simple counter for unique fragment IDs.
// FIXME: In a production system, you might want a thread-safe atomic counter.
var nextFragID uint16 = 0

func (s *Socket) sendFragmentPacket(sess *Session, data []byte) {
	if len(data) <= payloadMTU {
		return
	}

	fragID := nextFragID
	nextFragID++ // FIXME: Not thread-safe, for demonstration only.

	total := uint16(len(data) / payloadMTU)
	if len(data)%payloadMTU != 0 {
		total++
	}

	for i := uint16(0); i < total; i++ {
		start := int(i) * payloadMTU
		end := start + payloadMTU
		if end > len(data) {
			end = len(data)
		}

		nextSeq := sess.LastSeqNum + 1
		sess.LastSeqNum = nextSeq

		pkt := &Packet{
			SeqNum:    nextSeq,
			Data:      data[start:end],
			IsFrag:    true,
			FragID:    fragID,
			FragCount: total,
			FragIndex: i,
		}
		sess.SendWindow = append(sess.SendWindow, pkt)
		s.sendRaw(sess, pkt)
	}
}

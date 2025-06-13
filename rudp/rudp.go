package rudp

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

var (
	// RUDP_WINDOW is a sliding window size for in-flight unacknowledged packets.
	RUDP_WINDOW = 5
	// RUDP_TIMEOUT is the timeout duration for packet retransmission.
	RUDP_TIMEOUT = 500 * time.Millisecond
	// RUDP_MAX_RETRANS is the max number of retransmission attempts before giving up.
	RUDP_MAX_RETRANS = 5
)

const (
	EventDataReceived = "RUDP_EVENT_DATA"
	EventTimeout      = "RUDP_EVENT_TIMEOUT"
	EventClose        = "RUDP_EVENT_CLOSE"
)

// ErrMalformedPacket is returned when an incoming packet fails parsing (e.g., too short).
var ErrMalformedPacket = errors.New("malformed packet received")

// EventType represents an event type such as RUDP_EVENT_DATA.
type EventType string

// Event encapsulates an event, the session it belongs to, and optional payload.
type Event struct {
	Type    EventType
	Session *Session
	Data    []byte
}

// EventHandler is a callback for handling asynchronous events (e.g., incoming data, timeout).
type EventHandler func(event Event)

// Address represents an endpoint IP/port pair. TODO: (Currently unused in core logic—potential for extension).
type Address struct {
	IP   net.IP
	Port int
}

type Packet struct {
	SeqNum  uint32 // Packet sequence number.
	ACK     bool   // ACK flag (packet acknowledged).
	SYN     bool   // SYN flag (connection open).
	FIN     bool   // FIN flag (connection close).
	Data    []byte // Application payload.
	Retrans int    // Number of retransmissions so far.
}

type Session struct {
	PeerAddr   *net.UDPAddr           // Remote address.
	LastSeqNum uint32                 // Last used sequence number.
	SendWindow []*Packet              // In-flight, unacknowledged packets.
	SendQueue  []*Packet              // TODO: Unused.
	AckedUntil uint32                 // Last acknowledged sequence number.
	Timeouts   map[uint32]*time.Timer // Buffer of retransmission timers by sequence number.
	LastActive time.Time
	Closed     bool
	mu         sync.Mutex

	// fields for receive-side windowing
	ExpectedSeq uint32             // next expected sequence number
	RecvBuffer  map[uint32]*Packet // Buffer for out-of-order packets.
}

// Socket manages UDP transport, session demultiplexing, and event delivery.
type Socket struct {
	conn         *net.UDPConn
	sessions     map[string]*Session
	eventHandler EventHandler
	mu           sync.Mutex
}

// NewSocket initializes and binds a RUDP socket to addr with an event callback. Starts background listener.
func NewSocket(addr string, handler EventHandler) (*Socket, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	sock := &Socket{
		conn:         conn,
		sessions:     make(map[string]*Session),
		eventHandler: handler,
	}

	go sock.listen()
	return sock, nil
}

// Send transmits application data reliably using RUDP.
func (s *Socket) Send(sess *Session, data []byte) error {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	if sess.Closed {
		return errors.New("cannot send: session closed")
	}

	// Compute next sequence number
	var nextSeq uint32
	if len(sess.SendWindow) > 0 {
		lastPkt := sess.SendWindow[len(sess.SendWindow)-1]
		nextSeq = lastPkt.SeqNum + 1
	} else {
		nextSeq = sess.LastSeqNum + 1
	}
	sess.LastSeqNum = nextSeq

	// Create packet
	pkt := &Packet{
		SeqNum:  nextSeq,
		Data:    data,
		Retrans: 0,
	}

	// Send the packet (will handle retries and window updates)
	s.sendPacket(sess, pkt)
	return nil
}

func (s *Socket) listen() {
	buf := make([]byte, 2048)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		s.handlePacket(addr, buf[:n])
	}
}

func (s *Socket) handlePacket(addr *net.UDPAddr, data []byte) {
	packet, err := parsePacket(data)
	if err != nil {
		return
	}

	sessionKey := addr.String()
	s.mu.Lock()
	sess, exists := s.sessions[sessionKey]
	if !exists {
		if !packet.SYN {
			s.mu.Unlock()
			return
		}
		sess = &Session{
			PeerAddr:    addr,
			Timeouts:    make(map[uint32]*time.Timer),
			LastActive:  time.Now(),
			ExpectedSeq: packet.SeqNum + 1,
			RecvBuffer:  make(map[uint32]*Packet),
		}
		s.sessions[sessionKey] = sess
	}
	s.mu.Unlock()

	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.LastActive = time.Now()

	// Handle ACK
	if packet.ACK {
		if timer, ok := sess.Timeouts[packet.SeqNum-1]; ok {
			timer.Stop()
			delete(sess.Timeouts, packet.SeqNum-1)
		}
		for len(sess.SendWindow) > 0 && sess.SendWindow[0].SeqNum < packet.SeqNum {
			sess.SendWindow = sess.SendWindow[1:]
		}
		return
	}

	if packet.FIN {
		s.sendPacket(sess, ackPacket(packet.SeqNum+1))
		sess.Closed = true
		s.eventHandler(Event{Type: EventClose, Session: sess})
		return
	}

	if packet.SYN {
		s.sendPacket(sess, ackPacket(packet.SeqNum+1))
	}

	if len(packet.Data) > 0 {
		// Out-of-order buffer logic
		if packet.SeqNum < sess.ExpectedSeq {
			// Duplicate packet, ACK again
			s.sendPacket(sess, ackPacket(sess.ExpectedSeq))
			return
		} else if packet.SeqNum == sess.ExpectedSeq {
			// Deliver and slide window
			s.deliverInOrder(sess, packet)
		} else {
			// Buffer out-of-order packet
			sess.RecvBuffer[packet.SeqNum] = packet
		}
	}
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

// Close shuts down socket and notifies all sessions of closure.
func (s *Socket) Close() {
	s.conn.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.Closed = true
		s.eventHandler(Event{Type: EventClose, Session: sess})
	}
}

func (s *Socket) sendPacket(sess *Session, pkt *Packet) {
	buf := serializePacket(pkt)

	// Send the packet over UDP
	_, err := s.conn.WriteToUDP(buf, sess.PeerAddr)
	if err != nil {
		// Logging or retry logic could go here
		return
	}

	// Track in SendWindow if not an ACK
	if !pkt.ACK {
		sess.SendWindow = append(sess.SendWindow, pkt)

		if pkt.Retrans >= RUDP_MAX_RETRANS {
			// Give up and signal timeout
			s.eventHandler(Event{
				Type:    EventTimeout,
				Session: sess,
				Data:    pkt.Data,
			})
			return
		}

		// Schedule retransmission
		seq := pkt.SeqNum
		timer := time.AfterFunc(RUDP_TIMEOUT, func() {
			sess.mu.Lock()
			defer sess.mu.Unlock()

			// If already ACKed, skip
			if _, ok := sess.Timeouts[seq]; !ok {
				return
			}

			pkt.Retrans++
			delete(sess.Timeouts, seq)
			s.sendPacket(sess, pkt) // Recursive retry
		})
		sess.Timeouts[seq] = timer
	}
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

func (s *Socket) deliverInOrder(sess *Session, pkt *Packet) {
	for {
		payload := make([]byte, len(pkt.Data))
		copy(payload, pkt.Data)
		s.sendPacket(sess, ackPacket(pkt.SeqNum+1))
		s.eventHandler(Event{Type: EventDataReceived, Session: sess, Data: payload})

		sess.ExpectedSeq = pkt.SeqNum + 1

		// Look ahead before looping.
		nextPkt, ok := sess.RecvBuffer[sess.ExpectedSeq]
		if !ok {
			break
		}
		// Remove before proceeding to avoid reuse or overwrite.
		delete(sess.RecvBuffer, sess.ExpectedSeq)
		pkt = nextPkt
	}
}

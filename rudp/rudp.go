package rudp

import (
	"errors"
	"fmt"
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
	RUDP_MAX_RETRANS        = 5
	RUDP_RCV_BUFFER_SIZE    = 2048
	RUDP_SEND_LOOP_INTERVAL = 50 // In milliseconds
)

var sendACKs = true

const (
	EventDataReceived = "RUDP_EVENT_DATA"
	EventTimeout      = "RUDP_EVENT_TIMEOUT"
	EventClose        = "RUDP_EVENT_CLOSE"
	EventCreate       = "RUDP_EVENT_CREATE"
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

type SessionState int

const (
	Connecting SessionState = iota
	Established
	Closed
)

type Session struct {
	PeerAddr *net.UDPAddr // Remote address.
	state    SessionState
	mu       sync.Mutex

	// fields for receive-side windowing
	ExpectedSeq uint32             // next expected sequence number
	RecvBuffer  map[uint32]*Packet // Buffer for out-of-order packets.

	// fields for send-side windowing
	LastSeqNum uint32    // Last used sequence number.
	SendWindow []*Packet // In-flight, unacknowledged packets.
	AckedUntil uint32    // Last acknowledged sequence number.
	LastActive time.Time

	// Control channels
	established chan struct{}
	closed      chan struct{}
}

// Socket manages UDP transport, session demultiplexing, and event delivery.
type Socket struct {
	conn         *net.UDPConn
	sessions     map[string]*Session
	eventHandler EventHandler
	mu           sync.Mutex
}

// NewSocket initializes and binds a RUDP socket to addr with an event callback.
// Starts background listener.
func NewSocket(addr string, handler EventHandler) (*Socket, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	if handler == nil {
		handler = func(event Event) {}
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

	if sess.state == Closed {
		return errors.New("cannot send: session closed")
	}

	nextSeq := sess.LastSeqNum + 1
	sess.LastSeqNum = nextSeq

	// Create packet
	pkt := &Packet{
		SeqNum: nextSeq,
		Data:   data,
	}

	sess.SendWindow = append(sess.SendWindow, pkt)
	// Immediately try to send the new packet
	s.sendRaw(sess, pkt)
	return nil
}

func (s *Socket) sendLoop(sess *Session) {
	ticker := time.NewTicker(RUDP_TIMEOUT)
	defer ticker.Stop()

	for {
		select {
		case <-sess.closed:
			return
		case <-ticker.C:
			sess.mu.Lock()
			if sess.state == Closed {
				sess.mu.Unlock()
				return
			}
			var remaining []*Packet
			for _, pkt := range sess.SendWindow {
				if pkt.SeqNum < sess.AckedUntil {
					continue // Prune acknowledged packet
				}
				fmt.Printf("%s: Sending: %s\n", s.conn.LocalAddr().String(), pkt)
				if pkt.Retrans >= RUDP_MAX_RETRANS {
					s.eventHandler(Event{Type: EventTimeout, Session: sess, Data: pkt.Data})
					continue // Drop packet
				}
				s.sendRaw(sess, pkt)
				pkt.Retrans++
				remaining = append(remaining, pkt)
			}
			sess.SendWindow = remaining
			sess.mu.Unlock()
		}
	}
}

func (s *Socket) sendRaw(sess *Session, pkt *Packet) {
	buf := serializePacket(pkt)
	s.conn.WriteToUDP(buf, sess.PeerAddr)
}

func (s *Socket) Dial(addr string) (*Session, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	sess := newSession(udpAddr)
	s.mu.Lock()
	s.sessions[udpAddr.String()] = sess
	s.mu.Unlock()

	s.sendSYN(sess) // Place the SYN packet in the send window.
	go s.sendLoop(sess)

	select {
	case <-sess.established:
		// Success! The sendLoop is already running.
		return sess, nil
	case <-time.After(RUDP_TIMEOUT):
		// The dial timed out. We need to clean up the session and stop the sendLoop.
		sess.mu.Lock()
		// Check state to prevent race conditions if it gets established right at the timeout wire.
		if sess.state != Established {
			sess.state = Closed
			close(sess.closed) // Signal the sendLoop to stop.
		}
		sess.mu.Unlock()

		s.mu.Lock()
		delete(s.sessions, udpAddr.String())
		s.mu.Unlock()
		return nil, errors.New("dial timed out")
	}
}

func newSession(addr *net.UDPAddr) *Session {
	return &Session{
		PeerAddr:    addr,
		state:       Connecting,
		LastActive:  time.Now(),
		RecvBuffer:  make(map[uint32]*Packet),
		SendWindow:  make([]*Packet, 0), // FIXME: am I pointless?
		ExpectedSeq: 1,
		established: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

// SendTo sends data to the given remote address, establishing a session if needed.
func (s *Socket) SendTo(addr string, data []byte) error {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}

	key := udpAddr.String()

	s.mu.Lock()
	sess, exists := s.sessions[key]
	if !exists {
		sess = newSession(udpAddr)
		s.sessions[key] = sess
		s.mu.Unlock()

		// Initiate handshake outside lock
		s.sendSYN(sess)

		// Optional: wait briefly for handshake (or ACK)
		time.Sleep(20 * time.Millisecond)
	} else {
		s.mu.Unlock()
	}

	return s.Send(sess, data)
}

// sendSYN sends the initial SYN packet to begin a session.
func (s *Socket) sendSYN(sess *Session) {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	syn := &Packet{
		SeqNum:  1,
		SYN:     true,
		Retrans: 0,
	}
	sess.LastSeqNum = 1
	sess.SendWindow = append(sess.SendWindow, syn)
}

func (s *Socket) listen() {
	buf := make([]byte, RUDP_RCV_BUFFER_SIZE)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			// conn.Close() was likely called.
			return
		}
		// Make a copy of the data to avoid race conditions as buf is reused.
		data := make([]byte, n)
		copy(data, buf[:n])
		go s.handlePacket(addr, data)
	}
}

// handlePacket is the top-level dispatcher. It parses packets and routes them
// to the correct session for handling.
func (s *Socket) handlePacket(addr *net.UDPAddr, data []byte) {
	packet, err := parsePacket(data)
	if err != nil {
		// Malformed packet, ignore.
		return
	}

	sessionKey := addr.String()
	s.mu.Lock()
	sess, exists := s.sessions[sessionKey]
	if !exists {
		// If a session doesn't exist, we only care about SYN packets.
		if !packet.SYN {
			s.mu.Unlock()
			return
		}
		sess = newSession(addr)
		sess.ExpectedSeq = packet.SeqNum + 1 // Server expects next sequence after SYN.
		s.sessions[sessionKey] = sess
		s.mu.Unlock()

		s.eventHandler(Event{Type: EventCreate, Session: sess})
		go s.sendLoop(sess)
	} else {
		s.mu.Unlock()
	}

	// Delegate the actual packet handling to the session.
	events := sess.handle(packet, s)

	// Fire events after releasing all locks.
	for _, event := range events {
		s.eventHandler(event)
	}
}

// handle is the session-level packet handler. It manages state transitions
// and calls specific handlers based on packet type.
func (s *Session) handle(packet *Packet, sock *Socket) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.LastActive = time.Now()
	var eventsToFire []Event

	if s.state == Closed {
		return nil // Ignore packets for closed sessions.
	}

	if packet.ACK {
		// This is a SYN-ACK from the server, completing the handshake.
		if s.state == Connecting && packet.SYN {
			s.state = Established
			close(s.established) // Unblock Dial()
		}
		if packet.SeqNum > s.AckedUntil {
			s.AckedUntil = packet.SeqNum
		}
	} else if packet.FIN {
		if s.state != Closed {
			s.state = Closed
			close(s.closed) // Unblock sendLoop
			eventsToFire = append(eventsToFire, Event{Type: EventClose, Session: s})
		}
	} else if packet.SYN {
		// This is a SYN from a client. Server must respond with SYN-ACK.
		ack := &Packet{SeqNum: packet.SeqNum + 1, ACK: true, SYN: true}
		sock.sendRaw(s, ack)
	} else if len(packet.Data) > 0 {
		dataEvents := s.handleData(packet, sock)
		if dataEvents != nil {
			eventsToFire = append(eventsToFire, dataEvents...)
		}
	}
	return eventsToFire
}

// handleData manages the receive window, buffering out-of-order packets
// and preparing in-order packets for delivery.
func (s *Session) handleData(packet *Packet, sock *Socket) []Event {
	if packet.SeqNum < s.ExpectedSeq {
		// This is a duplicate packet. Acknowledge it again in case our last ACK was lost.
		sock.sendRaw(s, &Packet{SeqNum: s.ExpectedSeq, ACK: true})
		return nil
	}

	if packet.SeqNum > s.ExpectedSeq {
		// This is a future packet. Buffer it for later processing.
		s.RecvBuffer[packet.SeqNum] = packet
		return nil
	}

	// This is the expected packet. Process it and any subsequent contiguous packets.
	var eventsToFire []Event
	packetsToProcess := []*Packet{packet}
	s.ExpectedSeq++

	// Check buffer for the next in-order packets.
	for {
		nextPkt, ok := s.RecvBuffer[s.ExpectedSeq]
		if !ok {
			break // No more contiguous packets.
		}
		packetsToProcess = append(packetsToProcess, nextPkt)
		delete(s.RecvBuffer, s.ExpectedSeq)
		s.ExpectedSeq++
	}

	// Fire events and send ACKs for the processed packets.
	for _, p := range packetsToProcess {
		if sendACKs {
			sock.sendRaw(s, &Packet{SeqNum: p.SeqNum + 1, ACK: true})
		}
		payload := make([]byte, len(p.Data))
		copy(payload, p.Data)
		eventsToFire = append(eventsToFire, Event{Type: EventDataReceived, Session: s, Data: payload})
	}
	return eventsToFire
}

// Close shuts down the socket and all active sessions.
func (s *Socket) Close() {
	s.conn.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.mu.Lock()
		if sess.state != Closed {
			sess.state = Closed
			close(sess.closed) // Use close() for non-blocking broadcast.
			s.eventHandler(Event{Type: EventClose, Session: sess})
		}
		sess.mu.Unlock()
	}
}

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
	RUDP_MAX_RETRANS = 5
	// RUDP_RCV_BUFFER_SIZE is the size of the UDP receive buffer.
	RUDP_RCV_BUFFER_SIZE = 2048
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

// Address represents an endpoint IP/port pair.
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
				// Prune acknowledged packets
				if pkt.SeqNum < sess.AckedUntil {
					continue
				}

				// Check for timeout
				fmt.Printf("%s: Sending: %s\n", s.conn.LocalAddr().String(), pkt)
				if pkt.Retrans >= RUDP_MAX_RETRANS {
					s.eventHandler(Event{Type: EventTimeout, Session: sess, Data: pkt.Data})
					// Do not add to remaining, effectively dropping it.
					continue
				}

				// Retransmit
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

	// Place SYN in window and start the retransmission loop
	s.sendSYN(sess)
	go s.sendLoop(sess)

	// Wait for establishment or timeout
	select {
	case <-sess.established:
		return sess, nil
	case <-time.After(time.Duration(RUDP_MAX_RETRANS+1) * RUDP_TIMEOUT):
		sess.mu.Lock()
		if sess.state != Established {
			sess.state = Closed
			close(sess.closed)
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
		SendWindow:  make([]*Packet, 0),
		ExpectedSeq: 1,
		established: make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

// sendSYN sends the initial SYN packet to begin a session.
func (s *Socket) sendSYN(sess *Session) {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	syn := &Packet{
		SeqNum: 1,
		SYN:    true,
	}
	sess.LastSeqNum = 1
	sess.SendWindow = append(sess.SendWindow, syn)
	s.sendRaw(sess, syn) // Send immediately
}

func (s *Socket) listen() {
	buf := make([]byte, RUDP_RCV_BUFFER_SIZE)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		go s.handlePacket(addr, data)
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
		sess = newSession(addr)
		sess.ExpectedSeq = packet.SeqNum + 1
		s.sessions[sessionKey] = sess
		s.mu.Unlock()

		s.eventHandler(Event{Type: EventCreate, Session: sess})
		go s.sendLoop(sess)
	} else {
		s.mu.Unlock()
	}

	events := sess.handle(packet, s)

	for _, event := range events {
		s.eventHandler(event)
	}
}

func (s *Session) handle(packet *Packet, sock *Socket) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.LastActive = time.Now()
	var eventsToFire []Event

	if s.state == Closed {
		return nil
	}

	if packet.ACK {
		if s.state == Connecting && packet.SYN {
			s.state = Established
			close(s.established)
		}
		if packet.SeqNum > s.AckedUntil {
			s.AckedUntil = packet.SeqNum
		}
	} else if packet.FIN {
		if s.state != Closed {
			s.state = Closed
			close(s.closed)
			eventsToFire = append(eventsToFire, Event{Type: EventClose, Session: s})
		}
	} else if packet.SYN {
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

func (s *Session) handleData(packet *Packet, sock *Socket) []Event {
	if packet.SeqNum < s.ExpectedSeq && sendACKs {
		sock.sendRaw(s, &Packet{SeqNum: s.ExpectedSeq, ACK: true})
		return nil
	}

	if packet.SeqNum > s.ExpectedSeq {
		s.RecvBuffer[packet.SeqNum] = packet
		return nil
	}

	var eventsToFire []Event
	packetsToProcess := []*Packet{packet}
	s.ExpectedSeq++

	for {
		nextPkt, ok := s.RecvBuffer[s.ExpectedSeq]
		if !ok {
			break
		}
		packetsToProcess = append(packetsToProcess, nextPkt)
		delete(s.RecvBuffer, s.ExpectedSeq)
		s.ExpectedSeq++
	}

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

func (s *Socket) Close() {
	s.conn.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.mu.Lock()
		if sess.state != Closed {
			sess.state = Closed
			close(sess.closed)
			s.eventHandler(Event{Type: EventClose, Session: sess})
		}
		sess.mu.Unlock()
	}
}

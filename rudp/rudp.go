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
	ExpectedSeq  uint32             // next expected sequence number
	RecvBuffer   map[uint32]*Packet // Buffer for out-of-order packets.
	sendLoopQuit chan struct{}
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

	if sess.Closed {
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
	return nil
}

func (s *Socket) sendLoop(sess *Session) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-sess.sendLoopQuit:
			return
		case <-ticker.C:
			fmt.Printf("I am TICK\n")
			sess.mu.Lock()
			if sess.Closed {
				sess.mu.Unlock()
				return
			}
			// now := time.Now()
			var remaining []*Packet
			for _, pkt := range sess.SendWindow {
				fmt.Printf("Sending: %s\n", pkt)
				if pkt.Retrans >= RUDP_MAX_RETRANS {
					s.eventHandler(Event{
						Type:    EventTimeout,
						Session: sess,
						Data:    pkt.Data,
					})
					continue
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

	sess := &Session{
		PeerAddr:     udpAddr,
		Timeouts:     make(map[uint32]*time.Timer),
		LastActive:   time.Now(),
		RecvBuffer:   make(map[uint32]*Packet),
		ExpectedSeq:  1,
		sendLoopQuit: make(chan struct{}),
	}

	s.mu.Lock()
	s.sessions[udpAddr.String()] = sess
	s.mu.Unlock()

	s.sendSYN(sess)
	go s.sendLoop(sess)

	return sess, nil
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
		sess = &Session{
			PeerAddr:    udpAddr,
			Timeouts:    make(map[uint32]*time.Timer),
			LastActive:  time.Now(),
			RecvBuffer:  make(map[uint32]*Packet),
			ExpectedSeq: 1,
		}
		s.sessions[key] = sess
		s.mu.Unlock()

		// Initiate handshake outside lock
		if err := s.sendSYN(sess); err != nil {
			return err
		}

		// Optional: wait briefly for handshake (or ACK)
		time.Sleep(20 * time.Millisecond)
	} else {
		s.mu.Unlock()
	}

	return s.Send(sess, data)
}

func (s *Socket) sendSYN(sess *Session) error {
	// create a syn packet for the first communiaction.
	syn := &Packet{
		SeqNum:  1,
		SYN:     true,
		Retrans: 0,
	}
	// reset our last sequence number back to 1
	sess.LastSeqNum = 1
	sess.SendWindow = append(sess.SendWindow, syn)
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
	fmt.Printf("Received: %s from %s\n", packet, addr)

	sessionKey := addr.String()
	s.mu.Lock()
	sess, exists := s.sessions[sessionKey]
	if !exists {
		if !packet.SYN {
			s.mu.Unlock()
			return
		}
		sess = &Session{
			PeerAddr:     addr,
			LastActive:   time.Now(),
			ExpectedSeq:  packet.SeqNum + 1,
			RecvBuffer:   make(map[uint32]*Packet),
			sendLoopQuit: make(chan struct{}),
		}
		s.sessions[sessionKey] = sess
		s.eventHandler(Event{Type: EventCreate, Session: sess})
		go s.sendLoop(sess)
	}
	s.mu.Unlock()

	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.LastActive = time.Now()

	if packet.ACK {
		// Remove acknowledged packets from the send window
		var newWindow []*Packet
		for _, pkt := range sess.SendWindow {
			if pkt.SeqNum >= packet.SeqNum {
				newWindow = append(newWindow, pkt)
			}
		}
		sess.SendWindow = newWindow
		return
	}

	if packet.FIN {
		sess.Closed = true
		close(sess.sendLoopQuit)
		s.eventHandler(Event{Type: EventClose, Session: sess})
		return
	}

	if packet.SYN {
		ack := &Packet{SeqNum: packet.SeqNum + 1, ACK: true}
		s.sendRaw(sess, ack)
	}

	if !sendACKs {
		return
	}

	if len(packet.Data) > 0 {
		if packet.SeqNum < sess.ExpectedSeq {
			if sendACKs {
				s.sendRaw(sess, &Packet{SeqNum: sess.ExpectedSeq, ACK: true})
			}
			return
		} else if packet.SeqNum == sess.ExpectedSeq {
			payload := make([]byte, len(packet.Data))
			copy(payload, packet.Data)
			s.sendRaw(sess, &Packet{SeqNum: packet.SeqNum + 1, ACK: true})
			s.eventHandler(Event{Type: EventDataReceived, Session: sess, Data: payload})
			sess.ExpectedSeq++

			for {
				nextPkt, ok := sess.RecvBuffer[sess.ExpectedSeq]
				if !ok {
					break
				}
				delete(sess.RecvBuffer, sess.ExpectedSeq)
				payload := make([]byte, len(nextPkt.Data))
				copy(payload, nextPkt.Data)
				s.sendRaw(sess, &Packet{SeqNum: nextPkt.SeqNum + 1, ACK: true})
				s.eventHandler(Event{Type: EventDataReceived, Session: sess, Data: payload})
				sess.ExpectedSeq++
			}
		} else {
			sess.RecvBuffer[packet.SeqNum] = packet
		}
	}
}

// Close shuts down socket and notifies all sessions of closure.
func (s *Socket) Close() {
	s.conn.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.Closed = true
		sess.sendLoopQuit <- struct{}{} // close the send loop
		s.eventHandler(Event{Type: EventClose, Session: sess})
	}
}

// func (s *Socket) deliverInOrder(sess *Session, pkt *Packet) {
// 	for {
// 		payload := make([]byte, len(pkt.Data))
// 		copy(payload, pkt.Data)
// 		if sendACKs { // for testing purposes
// 			s.sendPacket(sess, ackPacket(pkt.SeqNum+1))
// 		}
// 		s.eventHandler(Event{Type: EventDataReceived, Session: sess, Data: payload})

// 		sess.ExpectedSeq = pkt.SeqNum + 1

// 		// Look ahead before looping.
// 		nextPkt, ok := sess.RecvBuffer[sess.ExpectedSeq]
// 		if !ok {
// 			break
// 		}
// 		// Remove before proceeding to avoid reuse or overwrite.
// 		delete(sess.RecvBuffer, sess.ExpectedSeq)
// 		pkt = nextPkt
// 	}
// }

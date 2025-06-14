package rudp

import (
	"errors"
	"math/rand"
	"net"
	"sync"
	"time"
)

var (
	// RUDP_WINDOW is a sliding window size for in-flight unacknowledged packets.
	RUDP_WINDOW = 100
	// RUDP_TIMEOUT is the timeout duration for packet retransmission.
	RUDP_TIMEOUT = 500 * time.Millisecond
	// RUDP_MAX_RETRANS is the max number of retransmission attempts before giving up.
	RUDP_MAX_RETRANS = 5
	// RUDP_RCV_BUFFER_SIZE is the size of the UDP receive buffer.
	RUDP_RCV_BUFFER_SIZE = 4096 // Increased buffer size for fragmentation
)

var sendACKs = true

// A simple counter for unique fragment IDs.
var fragIDMutex sync.Mutex

const (
	EventDataReceived = "RUDP_EVENT_DATA"
	EventTimeout      = "RUDP_EVENT_TIMEOUT"
	EventClose        = "RUDP_EVENT_CLOSE"
	EventCreate       = "RUDP_EVENT_CREATE"
)

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
	ExpectedSeq      uint32               // next expected sequence number
	RecvBuffer       map[uint32]*Packet   // Buffer for out-of-order packets.
	ReassemblyBuffer map[uint16][]*Packet // Maps FragID to a list of its received fragments

	// fields for send-side windowing
	LastSeqNum uint32    // Last used sequence number.
	nextFragID uint16    // Per-session counter for fragment IDs.
	SendWindow []*Packet // In-flight, unacknowledged packets.
	AckedUntil uint32    // Last acknowledged sequence number.
	LastActive time.Time

	// Control channels
	established chan struct{}
	closed      chan struct{}
}

func newSession(addr *net.UDPAddr) *Session {
	return &Session{
		PeerAddr:         addr,
		state:            Connecting,
		LastActive:       time.Now(),
		RecvBuffer:       make(map[uint32]*Packet),
		ReassemblyBuffer: make(map[uint16][]*Packet),
		SendWindow:       make([]*Packet, 0),
		ExpectedSeq:      1,
		nextFragID:       0,
		established:      make(chan struct{}),
		closed:           make(chan struct{}),
	}
}

// Socket manages UDP transport, session demultiplexing, and event delivery.
type Socket struct {
	conn         *net.UDPConn
	sessions     map[string]*Session
	eventHandler EventHandler
	mu           sync.Mutex
	// testPacketSendHook is a hook for testing to simulate packet loss.
	// If it returns false, the packet is not sent.
	testPacketSendHook func(pkt *Packet) bool
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
	rand.Seed(time.Now().UnixNano())
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

	if len(sess.SendWindow) >= RUDP_WINDOW {
		return errors.New("too many unacknowledged packets, wait to queue again")
	}

	if len(data) > payloadMTU {
		fragID := sess.nextFragID
		sess.nextFragID++

		totalFrags := uint16(len(data) / payloadMTU)
		if len(data)%payloadMTU != 0 {
			totalFrags++
		}

		for i := uint16(0); i < totalFrags; i++ {
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
				FragCount: totalFrags,
				FragIndex: i,
			}
			sess.SendWindow = append(sess.SendWindow, pkt)
			s.sendRaw(sess, pkt)
		}
	} else {
		// --- Original Send Logic ---
		nextSeq := sess.LastSeqNum + 1
		sess.LastSeqNum = nextSeq
		pkt := &Packet{
			SeqNum: nextSeq,
			Data:   data,
		}
		sess.SendWindow = append(sess.SendWindow, pkt)
		s.sendRaw(sess, pkt)
	}
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
					continue // Packet has been acknowledged
				}

				if pkt.Retrans >= RUDP_MAX_RETRANS {
					s.eventHandler(Event{Type: EventTimeout, Session: sess, Data: pkt.Data})
					continue // Drop the packet
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
	// Check test hook before sending
	if s.testPacketSendHook != nil {
		if !s.testPacketSendHook(pkt) {
			// Packet dropped by test hook.
			return
		}
	}
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

	s.sendSYN(sess)
	go s.sendLoop(sess)

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

func (s *Socket) sendSYN(sess *Session) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	syn := &Packet{SeqNum: 1, SYN: true}
	sess.LastSeqNum = 1
	sess.SendWindow = append(sess.SendWindow, syn)
	s.sendRaw(sess, syn)
}

func (s *Socket) listen() {
	buf := make([]byte, RUDP_RCV_BUFFER_SIZE)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return // Socket was likely closed
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		go s.handlePacket(addr, data)
	}
}

func (s *Socket) handlePacket(addr *net.UDPAddr, data []byte) {
	packet, err := parsePacket(data)
	if err != nil {
		return // Ignore malformed packet
	}

	sessionKey := addr.String()
	s.mu.Lock()
	sess, exists := s.sessions[sessionKey]
	if !exists {
		if !packet.SYN {
			s.mu.Unlock()
			return // Ignore non-SYN packets for unknown sessions
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
		return nil
	}

	if packet.FIN {
		if s.state != Closed {
			s.state = Closed
			close(s.closed)
			return []Event{{Type: EventClose, Session: s}}
		}
		return nil
	}

	if packet.SYN {
		// This is part of session establishment, handled in handlePacket
		ack := &Packet{SeqNum: packet.SeqNum + 1, ACK: true, SYN: true}
		sock.sendRaw(s, ack)
		return nil
	}

	if len(packet.Data) > 0 {
		return s.handleData(packet, sock)
	}

	return nil
}

// handleData processes incoming data packets, handling ordering and reassembly.
func (s *Session) handleData(packet *Packet, sock *Socket) []Event {
	// --- Packet Ordering Logic ---
	if packet.SeqNum < s.ExpectedSeq {
		// Duplicate packet, ACK and drop.
		if sendACKs {
			sock.sendRaw(s, &Packet{SeqNum: s.ExpectedSeq, ACK: true})
		}
		return nil
	}

	if packet.SeqNum > s.ExpectedSeq {
		// Future packet, buffer it.
		s.RecvBuffer[packet.SeqNum] = packet
		if sendACKs {
			// Send a cumulative ACK for what we have received so far,
			// not an ACK for the future packet. This signals to the sender
			// that we are still waiting for s.ExpectedSeq.
			sock.sendRaw(s, &Packet{SeqNum: s.ExpectedSeq, ACK: true})
		}
		return nil
	}

	// This is the packet we were waiting for. Gather it and any subsequent
	// contiguous packets from the buffer.
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

	// --- Processing and Reassembly Logic ---
	for _, p := range packetsToProcess {
		if p.IsFrag {
			fragments, exists := s.ReassemblyBuffer[p.FragID]
			if !exists {
				fragments = make([]*Packet, p.FragCount)
			}
			if p.FragIndex < uint16(len(fragments)) {
				fragments[p.FragIndex] = p
			}
			s.ReassemblyBuffer[p.FragID] = fragments

			allFragmentsReceived := true
			for _, frag := range fragments {
				if frag == nil {
					allFragmentsReceived = false
					break
				}
			}

			if allFragmentsReceived {
				// --- Reassemble the message ---
				var totalSize int
				for _, frag := range fragments {
					totalSize += len(frag.Data)
				}
				fullData := make([]byte, 0, totalSize)
				for _, frag := range fragments {
					fullData = append(fullData, frag.Data...)
				}
				delete(s.ReassemblyBuffer, p.FragID)

				// Fire event for the completed message
				payload := make([]byte, len(fullData))
				copy(payload, fullData)
				eventsToFire = append(eventsToFire, Event{Type: EventDataReceived, Session: s, Data: payload})
			}
		} else {
			// Not a fragment, fire event directly.
			payload := make([]byte, len(p.Data))
			copy(payload, p.Data)
			eventsToFire = append(eventsToFire, Event{Type: EventDataReceived, Session: s, Data: payload})
		}
	}

	// Send a single cumulative ACK for the highest sequence number processed.
	if sendACKs {
		sock.sendRaw(s, &Packet{SeqNum: s.ExpectedSeq, ACK: true})
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

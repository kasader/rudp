package rudp

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func buildPacket(seq uint32, flags byte, payload []byte) []byte {
	buf := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], seq)
	buf[4] = flags
	copy(buf[5:], payload)
	return buf
}

func TestParsePacket(t *testing.T) {
	data := buildPacket(42, 0x03, []byte("hi")) // SYN + ACK
	pkt, err := parsePacket(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pkt.SeqNum != 42 || !pkt.SYN || !pkt.Ack || pkt.FIN {
		t.Errorf("parsed packet mismatch: %+v", pkt)
	}
	if string(pkt.Data) != "hi" {
		t.Errorf("payload mismatch: %s", pkt.Data)
	}
}

func TestHandleSYNCreatesSession(t *testing.T) {
	events := make(chan Event, 1)

	// Use non-blocking send to avoid deadlock on Close()
	socket, err := NewSocket("127.0.0.1:0", func(evt Event) {
		select {
		case events <- evt:
		default:
			// Prevent blocking if no one is reading
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	// Prepare UDP connection to the test socket
	addr := &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: socket.conn.LocalAddr().(*net.UDPAddr).Port,
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatalf("failed to dial test socket: %v", err)
	}
	defer conn.Close()

	// Send SYN packet to initiate session
	seq := uint32(100)
	synPacket := buildPacket(seq, 0x02, nil) // 0x02 = SYN
	if _, err := conn.Write(synPacket); err != nil {
		t.Fatalf("failed to send SYN: %v", err)
	}

	// Wait up to 200ms for the session to be created
	var sessionCreated bool
	for i := 0; i < 10; i++ {
		time.Sleep(20 * time.Millisecond)

		socket.mu.Lock()
		sessionCreated = len(socket.sessions) > 0
		socket.mu.Unlock()

		if sessionCreated {
			break
		}
	}

	if !sessionCreated {
		t.Error("expected session to be created after sending SYN")
	}
}

func TestHandleDataTriggersReceiveEvent(t *testing.T) {
	recv := make(chan Event, 2)
	socket, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			recv <- evt
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	// Dial the socket
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: socket.conn.LocalAddr().(*net.UDPAddr).Port}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatalf("failed to dial socket: %v", err)
	}
	defer conn.Close()

	// Send SYN to initiate session
	if _, err := conn.Write(buildPacket(1, 0x02, nil)); err != nil {
		t.Fatalf("failed to send SYN: %v", err)
	}

	// Wait for session to be created
	var sessionCreated bool
	for i := 0; i < 10; i++ {
		time.Sleep(20 * time.Millisecond)
		socket.mu.Lock()
		sessionCreated = len(socket.sessions) > 0
		socket.mu.Unlock()
		if sessionCreated {
			break
		}
	}
	if !sessionCreated {
		t.Fatal("session was not created after sending SYN")
	}

	// Send data packet after session is confirmed
	if _, err := conn.Write(buildPacket(2, 0x00, []byte("Hello!"))); err != nil {
		t.Fatalf("failed to send data: %v", err)
	}

	// Loop: filter out any empty-payload events (from SYN)
	timeout := time.After(500 * time.Millisecond)
	for {
		select {
		case evt := <-recv:
			if string(evt.Data) == "Hello!" {
				// Success
				return
			}
			// Log and continue
			t.Logf("skipped event with unexpected payload: %q", evt.Data)
		case <-timeout:
			t.Fatal("timed out waiting for valid data event")
		}
	}
}

func TestRetransmissionTriggersTimeout(t *testing.T) {
	timeoutFired := make(chan struct{}, 1)

	socket, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventTimeout {
			timeoutFired <- struct{}{}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 50000}
	sess := &Session{
		PeerAddr: addr,
		Timeouts: make(map[uint32]*time.Timer),
	}
	socket.mu.Lock()
	socket.sessions[addr.String()] = sess
	socket.mu.Unlock()

	// Force retransmission immediately for test
	oldTimeout := RUDP_TIMEOUT
	RUDP_TIMEOUT = 10 * time.Millisecond
	defer func() { RUDP_TIMEOUT = oldTimeout }()

	socket.sendPacket(sess, &Packet{
		SeqNum:  1,
		Data:    []byte("test"),
		Retrans: RUDP_MAX_RETRANS,
	})

	select {
	case <-timeoutFired:
	case <-time.After(200 * time.Millisecond):
		t.Error("expected timeout event")
	}
}

func TestSendAPISendsData(t *testing.T) {
	recv := make(chan Event, 1)

	// Create receiver RUDP socket
	receiver, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			recv <- evt
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	// Create sender RUDP socket
	sender, err := NewSocket("127.0.0.1:0", func(evt Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	// Get receiver's address
	receiverAddr := receiver.conn.LocalAddr().(*net.UDPAddr)

	// Manually trigger SYN from sender to receiver to establish session
	synPkt := &Packet{
		SeqNum: 1,
		SYN:    true,
	}
	senderSess := &Session{
		PeerAddr:   receiverAddr,
		Timeouts:   make(map[uint32]*time.Timer),
		LastActive: time.Now(),
	}
	sender.mu.Lock()
	sender.sessions[receiverAddr.String()] = senderSess
	sender.mu.Unlock()

	sender.sendPacket(senderSess, synPkt)

	// Wait for receiver to establish the session
	var recvSess *Session
	for i := 0; i < 10; i++ {
		time.Sleep(20 * time.Millisecond)
		receiver.mu.Lock()
		for _, s := range receiver.sessions {
			recvSess = s
			break
		}
		receiver.mu.Unlock()
		if recvSess != nil {
			break
		}
	}
	if recvSess == nil {
		t.Fatal("receiver session not established")
	}

	// Send data from sender to receiver
	err = sender.Send(senderSess, []byte("ping from sender"))
	if err != nil {
		t.Fatalf("Send() failed: %v", err)
	}

	// Wait for data reception
	select {
	case evt := <-recv:
		if string(evt.Data) != "ping from sender" {
			t.Fatalf("unexpected payload: %q", evt.Data)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for data event")
	}
}

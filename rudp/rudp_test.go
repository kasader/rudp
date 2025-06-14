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
	if pkt.SeqNum != 42 || !pkt.SYN || !pkt.ACK || pkt.FIN {
		t.Errorf("parsed packet mismatch: %+v", pkt)
	}
	if string(pkt.Data) != "hi" {
		t.Errorf("payload mismatch: %s", pkt.Data)
	}
}

func TestHandleEventCreate(t *testing.T) {
	// Start a RUDP server socket
	var createdCh = make(chan struct{})
	server, err := NewSocket("127.0.0.1:0", func(event Event) {
		if event.Type == EventCreate {
			createdCh <- struct{}{}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Start a RUDP client and Dial the server
	client, err := NewSocket("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Dial the RUDP server socket
	_, err = client.Dial(server.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("failed to Dial server: %v", err)
	}

	// Wait up to 200ms for the session to be created on the server side
	select {
	case <-time.After(time.Millisecond * 200):
		t.Error("expected session to be created after dialing server")
	case <-createdCh:
		// Success
	}
}

func TestHandleEventDataReceived(t *testing.T) {
	dataCh := make(chan Event, 1)

	// Start a RUDP server socket
	server, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived && len(evt.Data) > 0 {
			dataCh <- evt
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Start a RUDP client socket
	client, err := NewSocket("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Dial the server to establish a session
	session, err := client.Dial(server.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	// Send data using the RUDP session
	payload := []byte("Hello!")
	if err := client.Send(session, payload); err != nil {
		t.Fatalf("failed to send data: %v", err)
	}

	// Expect the server to receive the data within a timeout
	select {
	case evt := <-dataCh:
		if string(evt.Data) != "Hello!" {
			t.Errorf("unexpected data: got %q, want %q", evt.Data, "Hello!")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected data event but timed out")
	}
}

func TestRetransmissionTriggersTimeout(t *testing.T) {
	timeoutFired := make(chan struct{}, 1)

	// Start server that does NOT respond to packets
	server, err := NewSocket("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Start client
	client, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventTimeout {
			timeoutFired <- struct{}{}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Dial server — this creates a session that the server will ignore
	session, err := client.Dial(server.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}

	// Reduce timeout to make test complete quickly
	originalTimeout := RUDP_TIMEOUT
	RUDP_TIMEOUT = 10 * time.Millisecond
	defer func() { RUDP_TIMEOUT = originalTimeout }()

	// Send data — server will not ACK it, triggering retries
	sendACKs = false
	defer func() { sendACKs = true }()
	client.Send(session, []byte("SOME_DATA"))

	// Expect a timeout event from the client
	select {
	case <-timeoutFired:
		// Success
	case <-time.After(500 * time.Millisecond):
		t.Error("expected timeout event but none occurred")
	}
}

func TestSendAPISendsData(t *testing.T) {
	recv := make(chan Event, 1)
	var payload = []byte("TEST_PAYLOAD")

	// Start server
	server, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			recv <- evt
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Start client
	client, err := NewSocket("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Dial the server
	session, err := client.Dial(server.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}

	client.Send(session, payload)

	// Wait for server to receive client payload
	select {
	case evt := <-recv:
		if string(evt.Data) != string(payload) {
			t.Errorf("unexpected data: got %q, want %q", evt.Data, payload)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for data event")
	}
}

func TestReceiveSideWindowing(t *testing.T) {
	recv := make(chan Event, 3)
	socket, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			recv <- evt
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: socket.conn.LocalAddr().(*net.UDPAddr).Port}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Send SYN
	conn.Write(buildPacket(1, 0x02, nil))
	time.Sleep(50 * time.Millisecond)

	// Send out-of-order packets: 3, 2, 1 (after SYN = seq 1)
	conn.Write(buildPacket(4, 0x00, []byte("third")))
	conn.Write(buildPacket(3, 0x00, []byte("second")))
	conn.Write(buildPacket(2, 0x00, []byte("first")))

	// Receive should yield: first, second, third in order
	expect := []string{"first", "second", "third"}
	for i := 0; i < 3; i++ {
		select {
		case evt := <-recv:
			if string(evt.Data) != expect[i] {
				t.Errorf("expected %q but got %q", expect[i], string(evt.Data))
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for packet %d", i)
		}
	}
}

// func TestFragmentationAndReassembly(t *testing.T) {
// 	received := make(chan Event, 1)

// 	receiver, err := NewSocket("127.0.0.1:0", func(evt Event) {
// 		if evt.Type == EventDataReceived {
// 			received <- evt
// 		}
// 	})
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	defer receiver.Close()

// 	sender, err := NewSocket("127.0.0.1:0", func(evt Event) {})
// 	if err != nil {
// 		t.Fatal(err)
// 	}
// 	defer sender.Close()

// 	receiverAddr := receiver.conn.LocalAddr().(*net.UDPAddr)

// 	sess := &Session{
// 		PeerAddr:   receiverAddr,
// 		Timeouts:   make(map[uint32]*time.Timer),
// 		LastActive: time.Now(),
// 		MTU:        200,
// 	}
// 	sender.mu.Lock()
// 	sender.sessions[receiverAddr.String()] = sess
// 	sender.mu.Unlock()

// 	// Establish session by sending SYN
// 	synPkt := &Packet{
// 		SeqNum: 1,
// 		SYN:    true,
// 	}
// 	sender.sendPacket(sess, synPkt)

// 	// Wait for receiver to accept session
// 	time.Sleep(100 * time.Millisecond)

// 	// Large payload to trigger fragmentation
// 	largeData := make([]byte, 1000)
// 	for i := range largeData {
// 		largeData[i] = byte(i % 256)
// 	}

// 	err = sender.Send(sess, largeData)
// 	if err != nil {
// 		t.Fatalf("Send failed: %v", err)
// 	}

// 	select {
// 	case evt := <-received:
// 		if len(evt.Data) != len(largeData) {
// 			t.Fatalf("expected %d bytes, got %d", len(largeData), len(evt.Data))
// 		}
// 		for i := range evt.Data {
// 			if evt.Data[i] != largeData[i] {
// 				t.Fatalf("data mismatch at byte %d", i)
// 			}
// 		}
// 	case <-time.After(1 * time.Second):
// 		t.Fatal("timed out waiting for reassembled message")
// 	}
// }

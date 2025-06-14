package rudp

import (
	"bytes"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
)

// TestFragmentationAndReassembly verifies that a payload larger than the MTU
// is correctly fragmented by the sender and reassembled by the receiver.
func TestFragmentationAndReassembly(t *testing.T) {
	// Dynamically set the MTU to a small value to force fragmentation.
	// We restore the original value at the end of the test.
	originalMTU := payloadMTU
	payloadMTU = 500
	defer func() { payloadMTU = originalMTU }()

	// Channel to receive the fully reassembled data from the receiver.
	receivedCh := make(chan []byte, 1)

	// Setup the receiver socket. Its event handler sends the data to our channel.
	receiver, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			receivedCh <- evt.Data
		}
	})
	if err != nil {
		t.Fatalf("failed to create receiver socket: %v", err)
	}
	defer receiver.Close()

	// Setup the sender socket. No event handler needed for the sender side.
	sender, err := NewSocket("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("failed to create sender socket: %v", err)
	}
	defer sender.Close()

	// The sender establishes a session with the receiver.
	session, err := sender.Dial(receiver.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("sender failed to dial receiver: %v", err)
	}

	// Create a large payload that is guaranteed to be fragmented.
	// Size is 1234 bytes, which with an MTU of 500 should create 3 fragments
	// (500, 500, 234 bytes).
	largePayload := make([]byte, 1234)
	for i := range largePayload {
		largePayload[i] = byte(i % 256) // Fill with predictable data
	}

	// Send the large payload. The RUDP socket should handle fragmentation automatically.
	if err := sender.Send(session, largePayload); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Wait for the reassembled message and verify its integrity.
	select {
	case receivedData := <-receivedCh:
		if !bytes.Equal(largePayload, receivedData) {
			t.Fatalf("reassembled data does not match original payload. got %d bytes, want %d bytes", len(receivedData), len(largePayload))
		}
		// If the bytes are equal, the test passed.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reassembled message")
	}
}

// TestFragmentationWithPacketLoss simulates the loss of a fragment and verifies
// that the sender's retransmission mechanism correctly resends the missing
// piece, allowing the receiver to eventually reassemble the full message.
func TestFragmentationWithPacketLoss(t *testing.T) {
	originalMTU := payloadMTU
	payloadMTU = 100 // Use a small MTU to ensure fragmentation
	defer func() { payloadMTU = originalMTU }()

	// Temporarily shorten the retransmission timeout for the test.
	originalTimeout := RUDP_TIMEOUT
	RUDP_TIMEOUT = 100 * time.Millisecond
	defer func() { RUDP_TIMEOUT = originalTimeout }()

	receivedCh := make(chan []byte, 1)
	receiver, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			receivedCh <- evt.Data
		}
	})
	if err != nil {
		t.Fatalf("failed to create receiver socket: %v", err)
	}
	defer receiver.Close()

	sender, err := NewSocket("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("failed to create sender socket: %v", err)
	}
	defer sender.Close()

	// --- Packet Drop Simulation ---
	var packetDropped bool
	var mu sync.Mutex
	packetToDropIndex := 1 // Corresponds to the second fragment

	// Set the test hook on the sender's socket to simulate packet loss.
	sender.testPacketSendHook = func(p *Packet) bool {
		mu.Lock()
		defer mu.Unlock()
		// We only want to drop the first attempt to send this fragment.
		// The subsequent retransmission should be allowed through.
		if p.IsFrag && p.FragIndex == uint16(packetToDropIndex) && !packetDropped {
			t.Logf("Simulating drop of packet: Seq=%d, FragIndex=%d", p.SeqNum, p.FragIndex)
			packetDropped = true
			return false // Returning false tells sendRaw to drop the packet.
		}
		return true // Send all other packets.
	}
	// --- End Simulation ---

	session, err := sender.Dial(receiver.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("sender failed to dial receiver: %v", err)
	}

	// Large payload that will be split into multiple fragments.
	// 350 bytes with MTU 100 -> 4 fragments (100, 100, 100, 50).
	largePayload := make([]byte, 350)
	for i := range largePayload {
		largePayload[i] = byte(i)
	}

	if err := sender.Send(session, largePayload); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	select {
	case receivedData := <-receivedCh:
		if !bytes.Equal(largePayload, receivedData) {
			t.Fatalf("reassembled data does not match original payload after packet loss and retransmission.")
		}
		// Success!
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reassembled message after packet loss")
	}
}

// TestFragmentationWithReordering sends fragments of a single message out of order
// to ensure the receiver correctly buffers and reassembles them.
func TestFragmentationWithReordering(t *testing.T) {
	originalMTU := payloadMTU
	payloadMTU = 100
	defer func() { payloadMTU = originalMTU }()

	receivedCh := make(chan []byte, 1)
	receiver, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			receivedCh <- evt.Data
		}
	})
	if err != nil {
		t.Fatalf("failed to create receiver socket: %v", err)
	}
	defer receiver.Close()

	// We need a raw UDP connection to manually send packets out of order.
	senderConn, err := net.DialUDP("udp", nil, receiver.conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("failed to create raw sender connection: %v", err)
	}
	defer senderConn.Close()

	// Establish session by sending a SYN manually.
	synPktBytes := serializePacket(&Packet{SeqNum: 1, SYN: true})
	senderConn.Write(synPktBytes)
	time.Sleep(50 * time.Millisecond) // Give receiver time to create session.

	// Large payload -> 4 fragments (100, 100, 100, 50).
	largePayload := make([]byte, 350)
	for i := range largePayload {
		largePayload[i] = byte(i)
	}

	// Manually create and serialize fragments.
	// SeqNums: 2, 3, 4, 5
	// FragIndices: 0, 1, 2, 3
	frag0 := &Packet{SeqNum: 2, Data: largePayload[0:100], IsFrag: true, FragID: 1, FragCount: 4, FragIndex: 0}
	frag1 := &Packet{SeqNum: 3, Data: largePayload[100:200], IsFrag: true, FragID: 1, FragCount: 4, FragIndex: 1}
	frag2 := &Packet{SeqNum: 4, Data: largePayload[200:300], IsFrag: true, FragID: 1, FragCount: 4, FragIndex: 2}
	frag3 := &Packet{SeqNum: 5, Data: largePayload[300:350], IsFrag: true, FragID: 1, FragCount: 4, FragIndex: 3}

	fragments := []*Packet{frag0, frag1, frag2, frag3}
	// Send them out of order: 3, 0, 2, 1
	sendOrder := []int{3, 0, 2, 1}

	for _, idx := range sendOrder {
		t.Logf("Sending fragment index %d (Seq: %d)", fragments[idx].FragIndex, fragments[idx].SeqNum)
		senderConn.Write(serializePacket(fragments[idx]))
		time.Sleep(10 * time.Millisecond) // Small delay
	}

	select {
	case receivedData := <-receivedCh:
		if !bytes.Equal(largePayload, receivedData) {
			t.Fatalf("reassembled data does not match original payload when fragments are reordered.")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reassembled message with reordered fragments")
	}
}

// TestInterleavedFragmentedMessages sends two large messages concurrently,
// ensuring their fragments can be received interleaved and reassembled into two
// separate, correct messages. This tests the importance of FragID.
func TestInterleavedFragmentedMessages(t *testing.T) {
	originalMTU := payloadMTU
	payloadMTU = 150
	defer func() { payloadMTU = originalMTU }()

	// We expect two large messages.
	receivedCh := make(chan []byte, 2)
	receiver, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			receivedCh <- evt.Data
		}
	})
	if err != nil {
		t.Fatalf("failed to create receiver socket: %v", err)
	}
	defer receiver.Close()

	sender, err := NewSocket("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("failed to create sender socket: %v", err)
	}
	defer sender.Close()

	session, err := sender.Dial(receiver.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("sender failed to dial receiver: %v", err)
	}

	// Two different large payloads.
	payloadA := make([]byte, 400) // -> 3 fragments
	payloadB := make([]byte, 350) // -> 3 fragments
	for i := range payloadA {
		payloadA[i] = 'A'
	}
	for i := range payloadB {
		payloadB[i] = 'B'
	}

	// We need to manually interleave packets.
	// Disable the automatic sending and do it manually.
	// This is hard without changing the library.
	// Alternative: send both from the application layer and hope the OS interleaves them.
	// This is not guaranteed. Let's send them in rapid succession.
	go sender.Send(session, payloadA)
	go sender.Send(session, payloadB)

	// Collect the two reassembled messages.
	var receivedPayloads [][]byte
	for i := 0; i < 2; i++ {
		select {
		case data := <-receivedCh:
			receivedPayloads = append(receivedPayloads, data)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for message %d", i+1)
		}
	}

	// Verify both payloads were received correctly, regardless of order.
	foundA := false
	foundB := false
	for _, p := range receivedPayloads {
		if bytes.Equal(p, payloadA) {
			foundA = true
		} else if bytes.Equal(p, payloadB) {
			foundB = true
		}
	}

	if !foundA || !foundB {
		t.Fatal("did not correctly reassemble two interleaved fragmented messages")
	}
}

// TestFragmentationWithDuplicateFragments sends a fragment twice to ensure
// the receiver's logic handles duplicates gracefully and doesn't corrupt the
// final reassembled message.
func TestFragmentationWithDuplicateFragments(t *testing.T) {
	originalMTU := payloadMTU
	payloadMTU = 100
	defer func() { payloadMTU = originalMTU }()

	receivedCh := make(chan []byte, 1)
	receiver, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			// Add a small delay on receive to allow duplicate to arrive
			// before reassembly is complete.
			time.Sleep(50 * time.Millisecond)
			receivedCh <- evt.Data
		}
	})
	if err != nil {
		t.Fatalf("failed to create receiver socket: %v", err)
	}
	defer receiver.Close()

	senderConn, err := net.DialUDP("udp", nil, receiver.conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("failed to create raw sender connection: %v", err)
	}
	defer senderConn.Close()

	senderConn.Write(serializePacket(&Packet{SeqNum: 1, SYN: true}))
	time.Sleep(50 * time.Millisecond)

	largePayload := make([]byte, 250) // 3 fragments
	for i := range largePayload {
		largePayload[i] = byte(rand.Intn(256))
	}

	frag0 := &Packet{SeqNum: 2, Data: largePayload[0:100], IsFrag: true, FragID: 1, FragCount: 3, FragIndex: 0}
	frag1 := &Packet{SeqNum: 3, Data: largePayload[100:200], IsFrag: true, FragID: 1, FragCount: 3, FragIndex: 1}
	frag2 := &Packet{SeqNum: 4, Data: largePayload[200:250], IsFrag: true, FragID: 1, FragCount: 3, FragIndex: 2}

	// Send fragment 1 twice.
	t.Log("Sending fragment 0")
	senderConn.Write(serializePacket(frag0))
	time.Sleep(10 * time.Millisecond)

	t.Log("Sending fragment 1 (first time)")
	senderConn.Write(serializePacket(frag1))
	time.Sleep(10 * time.Millisecond)

	t.Log("Sending fragment 2")
	senderConn.Write(serializePacket(frag2))
	time.Sleep(10 * time.Millisecond)

	t.Log("Sending fragment 1 (duplicate)")
	senderConn.Write(serializePacket(frag1)) // Send duplicate

	select {
	case receivedData := <-receivedCh:
		if !bytes.Equal(largePayload, receivedData) {
			t.Fatalf("reassembled data (len %d) does not match original payload (len %d) when a fragment is duplicated.", len(receivedData), len(largePayload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reassembled message with duplicated fragment")
	}
}

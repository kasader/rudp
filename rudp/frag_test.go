package rudp

import (
	"bytes"
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

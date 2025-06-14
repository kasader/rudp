package rudp

import (
	"bytes"
	crand "crypto/rand"
	"math/rand/v2"
	"testing"
	"time"
)

func TestFuzzedEchoRoundTrip(t *testing.T) {
	const numMessages = 4
	const maxPayloadSize = 256
	const timeout = 500 * time.Millisecond

	echoCh := make(chan Event, numMessages)

	// Start echo server
	var server *Socket
	sendACKs = true

	// Start echo server
	server, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived && evt.Session != nil {
			// fmt.Printf("SERVER: ExpectedSeq=%d, received packet Seq=%d\n", evt.Session.ExpectedSeq, evt.Session.LastSeqNum)
			server.Send(evt.Session, evt.Data)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Start client
	client, err := NewSocket("127.0.0.1:0", func(evt Event) {
		if evt.Type == EventDataReceived {
			echoCh <- evt
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Dial the server
	session, err := client.Dial(server.conn.LocalAddr().String())
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}

	// Generate and send fuzzed payloads
	sentPayloads := make([][]byte, 0, numMessages)
	for i := 0; i < numMessages; i++ {
		size := 1 + rand.IntN(maxPayloadSize-1)
		payload := make([]byte, size)
		crand.Read(payload)
		sentPayloads = append(sentPayloads, payload)

		if err := client.Send(session, payload); err != nil {
			t.Fatalf("failed to send payload %d: %v", i, err)
		}
	}

	// Verify echoed responses match sent payloads
	for i := 0; i < numMessages; i++ {
		select {
		case evt := <-echoCh:
			expected := sentPayloads[i]
			if !bytes.Equal(evt.Data, expected) {
				t.Errorf("echo mismatch on message %d: got %x, want %x", i, evt.Data, expected)
			}
		case <-time.After(timeout):
			t.Fatalf("timed out waiting for echo message %d", i)
		}
	}
}

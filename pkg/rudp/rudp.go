package rudp

// import "net"

// https://github.com/ms-s/rudp
// https://github.com/CBenoit/RUDP
// https://en.wikipedia.org/wiki/QUIC

// RUDP should also be robust to handle both cases of endianness.

// There are (3) types of events:
// 1. Trigger when data is seceived on a RUDP socket.
//    This is to pass received data from the RUDP socket to the application.
//    This is nice because it is very similar to the implementation at work!
//
// 2. Trigger when packet loss is detected (via a timeout event). RUDP_EVENT_TIMEOUT, which indicates
//    that a packet has been transported more than RUDP_MAX_RETRANS.
//
// 3. RUDP_EVENT_CLOSE i.e. the socket has been closed.

// Each RUDP session should be identified uniquely by the IP address and the port of the peer w/
// whom a a session is established.

// - RUDP relies on sequence numbers to provide reliability. An RUDP sequence number is an unsigned 32-bit interger
//   which is transmitted as a field in the RUDP header.
// - When we send a SYN its sequence number is randomly generated (why?)
// - Packets sent after the SYN are sent with incremented sequence numbers.
// - ACK packets have a sequence number which is 1 greater than the sequence number of the packet they acknowledge.
// - Also, when comparing sequence numbers, macros are used which handle the multiple cases of potential interger overflow.

// Sliding Window Logic:
//
// RUDP sender sessions maintain a sliding window of transmitted but unacknowledged packets. The size of
// this sliding window is defined by RUDP_WINDOW. When the application provides RUDP with data to be sent,
// we determine whether any slots in the sliding window are open. If so, the packet can immediately be added
// to the window and transmitted. If not, we must queue the packet to be delivered once it can acquire a slot
// in the window. Upon receiving an ACK packet, we inspect the first item in the sliding window. If the ACK
// packet is intended to acknowledge the first window item, we remove this item from the sliding window and
// shift any subsequent window items to the left, creating space in the window for new packets to be sent.
// As long as RUDP_WINDOW is greater than 1, this scheme provides better efficiency than stop-and-wait flow
// control by allowing up to RUDP_WINDOW outstanding unacknowledged packets to be sent.

// When a non-ACK packet is sent in RUDP, a timer event is registered to occur after RUDP_TIMEOUT milliseconds.
// If that timeout event fires, the packet associated with it will be restransmitted unless the packet has already
// been transmitted RUDP_MAX_RETRANS times. In that case, we will trigger a RUDP_EVENT_TIMEOUT event.
//
// When an ACK is recieved for a packet, the timeout event for that corresponding packet is canceled.
//
// In RUDP, timeout events represent the detection of packet loss.
// Since we do not utilize negative acknowledgments, we instead detect packet loss implicitly when an ACK is not received.
//
// TODO: What is a negative acknowledgement?

// When the RUDP socket is to be closed, they attempt to close all sessions which exist on the socket.
// For each (active) sender session on the socket, we wait until all queued data has been successfully transmitted,
// after which we send a FIN message.
// We wait to close the socket until the corresponding ACK to the FIN is received.
// We wait until all sessions have been closed before firing a handler on RUDP_EVENT_CLOSE event.

// There are some other high-level networking features such as "authentication, lobbying, server discovery, encryption"
// that are possible to add into an RUDP implementation as well. However, as for what these are is a little unclear.

const (
	maxRetries uint8 = 5
	maxClients uint8 = 32 // this is done on the server side.
)

type address struct {
	host string
	port uint16
}

// There need to be several events on the server-side such as:
// EVENT_TYPE_CONNECT (new connection)
// EVENT_TYPE_RECEIVE (data packet received)
// EVENT_TYPE_DISCONNECT (connection ended)
// EVENT_TYPE_DISCONNECT_TIMEOUT (disconnect due to timeout)

type server struct{}
type client struct{}

// func connect() error {

// 	// Resolve server address once
// 	serverAddr, err := net.ResolveUDPAddr("udp", addr)
// 	if err != nil {
// 		return err
// 	}

// 	localAddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
// 	conn, err := net.ListenUDP("udp", localAddr)
// 	if err != nil {
// 		return err
// 	}

// }

func main() {

}

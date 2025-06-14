package rudp

var (
	// Max payload size before fragmentation.
	// FIXME: The commented-out test used 200, so let's start there.
	// Note: Real MTU is around 1400-1500 bytes.
	payloadMTU = 500
)

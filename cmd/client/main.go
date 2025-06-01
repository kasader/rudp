package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
)

func main() {
	rcvBuf := make([]byte, 2048)
	conn, err := net.Dial("udp", "127.0.0.1:7777")
	defer conn.Close()
	if err != nil {
		fmt.Printf("Error: %v", err)
		os.Exit(1)
	}
	fmt.Fprintf(conn, "Message from RUDP client.")
	_, err = bufio.NewReader(conn).Read(rcvBuf)
	if err == nil {
		fmt.Printf("%s\n", rcvBuf)
	} else {
		fmt.Printf("Some error %v\n", err)
	}
}

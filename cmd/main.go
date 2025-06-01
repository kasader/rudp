package main

import (
	"fmt"
	"net"
)

func main() {
	s, err := net.LookupAddr("64.233.170.93")
	if err != nil {
		fmt.Printf(err.Error())
	} else {
		fmt.Println(s)
	}

	z, err := net.LookupCNAME("pkg.go.dev/net")
	if err != nil {
		fmt.Printf(err.Error())
	} else {
		fmt.Println(z)
	}
}

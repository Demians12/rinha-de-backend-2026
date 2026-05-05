// healthcheck: verifica se o servidor UDS está aceitando conexões
// Usado pelo docker-compose healthcheck
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <socket-path>")
		os.Exit(1)
	}
	conn, err := net.DialTimeout("unix", os.Args[1], 500*time.Millisecond)
	if err != nil {
		os.Exit(1)
	}
	conn.Close()
	os.Exit(0)
}

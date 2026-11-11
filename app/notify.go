package app

import (
	"fmt"
	"net"
)

// NotifyFunc returns a function that sends a state such as "READY=1" to the service
// manager over the datagram socket socketPath, the value of $NOTIFY_SOCKET. A leading
// "@" names a socket in the Linux abstract namespace. An empty socketPath, which is how
// the variable looks when no service manager is waiting, gives a function that does
// nothing.
func NotifyFunc(socketPath string) func(state string) error {
	if socketPath == "" {
		return func(string) error { return nil }
	}
	return func(state string) error {
		conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socketPath, Net: "unixgram"})
		if err != nil {
			return fmt.Errorf("connect to the service manager: %w", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := conn.Write([]byte(state)); err != nil {
			return fmt.Errorf("notify the service manager: %w", err)
		}
		return nil
	}
}

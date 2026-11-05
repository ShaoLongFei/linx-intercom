package websocketurl

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

func Validate(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse websocket URL: %w", err)
	}
	if !u.IsAbs() || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("websocket URL must be absolute and cannot contain userinfo or a fragment")
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if (ip != nil && ip.IsLoopback()) || strings.EqualFold(host, "localhost") {
			return nil
		}
		return fmt.Errorf("remote websocket URL must use wss")
	default:
		return fmt.Errorf("unsupported websocket URL scheme %q", u.Scheme)
	}
}

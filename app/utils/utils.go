package utils

import (
	"net"
	"strings"
)

func ExtractPort(addr string) string {

	_, port, err := net.SplitHostPort(addr)

	if err != nil {

		// If it fails, maybe it's just a port
		if strings.Contains(addr, ":") {
			return "" // invalid format
		}

		return addr

	}

	return port

}

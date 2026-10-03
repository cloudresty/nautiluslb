//go:build linux

package tcpproxy

import "golang.org/x/sys/unix"

const keepIdleOpt = unix.TCP_KEEPIDLE

package proxy

import "syscall"

// Winsock reports its own codes, which syscall.ECONNREFUSED and ECONNRESET do not match.
const (
	errConnRefused = syscall.Errno(10061) // WSAECONNREFUSED
	errConnReset   = syscall.WSAECONNRESET
)

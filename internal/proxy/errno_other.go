//go:build !windows

package proxy

import "syscall"

const (
	errConnRefused = syscall.ECONNREFUSED
	errConnReset   = syscall.ECONNRESET
)

package main

import (
	"fmt"
	"os"

	"github.com/kuasar-sandbox/sandboxer/pkg/readiness"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"golang.org/x/sys/unix"
)

// newReadinessFDWriter adopts the inherited readiness descriptor. Event framing
// and closure after notification are shared with SDK integrations. A non-nil
// timing upgrades the ready line to "ready\t{json}" with sandbox-ctl/CH/restore
// instants; nil keeps the historical plain wire.
func newReadinessFDWriter(fd int, logf func(string, ...any), timing *sandbox.ReadinessTiming) (*readiness.Writer, error) {
	if fd < 3 {
		return nil, fmt.Errorf("--ready-fd must be at least 3, got %d", fd)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return nil, fmt.Errorf("--ready-fd=%d is invalid: %w", fd, err)
	}
	// Keep the descriptor out of Cloud Hypervisor and every other later exec.
	// node-ctl clears CLOEXEC only for its own exec into sandbox-ctl; ownership
	// begins here and the bit is restored immediately.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return nil, fmt.Errorf("set FD_CLOEXEC on --ready-fd=%d: %w", fd, err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("ready-fd-%d", fd))
	if f == nil {
		return nil, fmt.Errorf("wrap --ready-fd=%d", fd)
	}
	w := readiness.NewWriter(f, logf)
	w.SetTiming(timing)
	return w, nil
}

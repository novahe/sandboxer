package uffd

import (
	"bytes"
	"testing"
	"unsafe"
)

// recordingDataSource models a local-file data run. It deliberately fills the
// caller's buffer so the test can verify which mapping handleFault supplied as
// the ReadAt destination.
type recordingDataSource struct {
	fill byte
	buf  []byte
	off  uint64
}

func (s *recordingDataSource) ReadAt(buf []byte, off uint64) (int, bool, error) {
	s.buf = buf
	s.off = off
	for i := range buf {
		buf[i] = s.fill
	}
	return len(buf), false, nil
}

func TestHandleFaultDirectPopulateReadsIntoBackendAndSkipsCopy(t *testing.T) {
	const (
		pages = 2
		chVA  = 0x100000
	)

	backend := make([]byte, pages*PageSize)
	pageBuf := bytes.Repeat([]byte{0xcc}, MaxBatchBytes)
	source := &recordingDataSource{fill: 0x5a}
	addrMap := NewAddressMap(uint64(len(backend)))
	if err := addrMap.RegisterVMA(ProcessCH, chVA, uint64(len(backend)), 0); err != nil {
		t.Fatalf("RegisterVMA(CH): %v", err)
	}

	h := &Handler{
		cfg: Config{
			BackendVA:      uintptr(unsafe.Pointer(&backend[0])),
			DirectPopulate: true,
			Size:           len(backend),
			Source:         source,
		},
		addrMap: addrMap,
		state:   NewPageStateMap(pages),
		logf:    func(string, ...any) {},
	}
	var gotWake struct {
		fd, start, length uint64
		calls             int
	}
	h.wakeFn = func(fd int, start, length uint64) error {
		gotWake.fd = uint64(fd)
		gotWake.start = start
		gotWake.length = length
		gotWake.calls++
		return nil
	}

	h.handleFault(faultEvent{address: chVA, uffdFD: 7}, pageBuf)

	if source.off != 0 {
		t.Fatalf("ReadAt offset = %d, want 0", source.off)
	}
	if len(source.buf) != len(backend) {
		t.Fatalf("ReadAt buffer length = %d, want %d", len(source.buf), len(backend))
	}
	if len(source.buf) == 0 || unsafe.Pointer(&source.buf[0]) != unsafe.Pointer(&backend[0]) {
		t.Fatal("direct populate did not pass the backend mapping to Source.ReadAt")
	}
	if !bytes.Equal(backend, bytes.Repeat([]byte{source.fill}, len(backend))) {
		t.Fatal("snapshot bytes were not written directly into the backend mapping")
	}
	if !bytes.Equal(pageBuf, bytes.Repeat([]byte{0xcc}, len(pageBuf))) {
		t.Fatal("direct populate unexpectedly used the worker copy buffer")
	}
	if h.state.Get(0) != StateLoaded || h.state.Get(1) != StateLoaded {
		t.Fatalf("directly populated pages not marked loaded: %v, %v", h.state.Get(0), h.state.Get(1))
	}
	stats := h.Stats()
	if stats["copy_calls"] != 0 || stats["pages_copied"] != 0 {
		t.Fatalf("direct populate used UFFDIO_COPY: copy_calls=%d pages_copied=%d", stats["copy_calls"], stats["pages_copied"])
	}
	if stats["pages_direct"] != pages {
		t.Fatalf("directly populated pages = %d, want %d", stats["pages_direct"], pages)
	}
	if gotWake.calls != 1 || gotWake.fd != 7 || gotWake.start != chVA || gotWake.length != uint64(len(backend)) {
		t.Fatalf("WAKE = calls=%d fd=%d start=%#x length=%d, want calls=1 fd=7 start=%#x length=%d",
			gotWake.calls, gotWake.fd, gotWake.start, gotWake.length, chVA, len(backend))
	}
	if stats["wakes"] != 1 {
		t.Fatalf("successful direct WAKE count = %d, want 1", stats["wakes"])
	}
}

func TestNewWithBackendUffdDirectPopulateRequiresBackendMapping(t *testing.T) {
	_, err := NewWithBackendUffd(1, NewAddressMap(PageSize), Config{
		MemfdFD:        1,
		Size:           PageSize,
		DirectPopulate: true,
		Source:         ZeroSource{},
	})
	if err == nil || err.Error() != "uffd: DirectPopulate requires BackendVA" {
		t.Fatalf("NewWithBackendUffd error = %v, want DirectPopulate backend validation", err)
	}
}

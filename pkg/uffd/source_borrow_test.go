package uffd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

type borrowingTestStream struct {
	data        []byte
	borrowErr   error
	readCalls   int
	borrowCalls int
}

func (s *borrowingTestStream) Size() uint64 { return uint64(len(s.data)) }

func (s *borrowingTestStream) RunAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	if offset >= uint64(len(s.data)) {
		return 0, 0, io.EOF
	}
	end := offset + limit
	if end > uint64(len(s.data)) {
		end = uint64(len(s.data))
	}
	return sparse.Data, end, nil
}

func (s *borrowingTestStream) ReadAt(_ context.Context, buf []byte, offset uint64) (int, error) {
	s.readCalls++
	return copy(buf, s.data[offset:]), nil
}

func (s *borrowingTestStream) BorrowAt(offset, limit uint64) ([]byte, bool, error) {
	s.borrowCalls++
	if s.borrowErr != nil {
		return nil, false, s.borrowErr
	}
	end := offset + limit
	if end > uint64(len(s.data)) {
		return nil, false, nil
	}
	return s.data[offset:end], true, nil
}

func (*borrowingTestStream) Close() error { return nil }

func TestStreamSnapshotSourceBorrowAtSingleLayerIdentity(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, 2*PageSize)
	stream := &borrowingTestStream{data: data}
	source, err := NewStreamSnapshotSource(
		context.Background(),
		fetch.NewLayered(stream),
		uint64(len(data)),
		StreamSnapshotSourceOptions{AllowFileBorrow: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !source.BorrowingEnabled() {
		t.Fatal("borrowing capability was not detected")
	}

	borrowed, ok, err := source.BorrowAt(0, uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("BorrowAt did not select the local-stream fast path")
	}
	if len(borrowed) != len(data) || &borrowed[0] != &data[0] {
		t.Fatal("BorrowAt did not return the stream's original data view")
	}
	if stream.readCalls != 0 || stream.borrowCalls != 1 {
		t.Fatalf("calls: ReadAt=%d BorrowAt=%d", stream.readCalls, stream.borrowCalls)
	}
}

func TestReadFaultSourceBorrowAtNonZeroOffsetStartsAtFaultPage(t *testing.T) {
	data := append(
		bytes.Repeat([]byte{0x11}, PageSize),
		bytes.Repeat([]byte{0x22}, 2*PageSize)...,
	)
	stream := &borrowingTestStream{data: data}
	source, err := NewStreamSnapshotSource(
		context.Background(),
		stream,
		uint64(len(data)),
		StreamSnapshotSourceOptions{AllowFileBorrow: true},
	)
	if err != nil {
		t.Fatal(err)
	}

	scratch := make([]byte, 2*PageSize)
	copyBuf, n, zero, borrowed, borrowErr, err := readFaultSource(source, scratch, PageSize)
	if err != nil {
		t.Fatal(err)
	}
	if borrowErr != nil {
		t.Fatalf("BorrowAt error = %v", borrowErr)
	}
	if !borrowed || zero || n != len(scratch) {
		t.Fatalf("borrow = n %d, zero %v, borrowed %v", n, zero, borrowed)
	}
	if &copyBuf[0] != &data[PageSize] {
		t.Fatal("copyBuf[0] does not correspond to the non-zero fault offset")
	}
	if copyBuf[0] != 0x22 {
		t.Fatalf("copyBuf[0] = %#x, want fault-page byte 0x22", copyBuf[0])
	}
}

func TestStreamSnapshotSourceBorrowAtLayeredFallsBack(t *testing.T) {
	data := bytes.Repeat([]byte{0x6b}, PageSize)
	top := &borrowingTestStream{data: data}
	base := &borrowingTestStream{data: data}
	source, err := NewStreamSnapshotSource(
		context.Background(),
		fetch.NewLayered(top, base),
		uint64(len(data)),
		StreamSnapshotSourceOptions{AllowFileBorrow: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.BorrowingEnabled() {
		t.Fatal("layered stream incorrectly exposes borrowing")
	}

	if borrowed, ok, err := source.BorrowAt(0, PageSize); err != nil || ok || borrowed != nil {
		t.Fatalf("layered BorrowAt = len %d, ok %v, err %v", len(borrowed), ok, err)
	}
	scratch := make([]byte, PageSize)
	copyBuf, n, zero, borrowed, borrowErr, err := readFaultSource(source, scratch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if borrowErr != nil {
		t.Fatalf("BorrowAt error = %v", borrowErr)
	}
	if borrowed || zero || n != len(scratch) || &copyBuf[0] != &scratch[0] {
		t.Fatalf("fallback = n %d, zero %v, borrowed %v", n, zero, borrowed)
	}
	if !bytes.Equal(copyBuf, data) {
		t.Fatal("buffered fallback returned wrong data")
	}
}

func TestStreamSnapshotSourceBorrowAtDisabledByDefault(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, PageSize)
	stream := &borrowingTestStream{data: data}
	source, err := NewStreamSnapshotSource(
		context.Background(),
		stream,
		uint64(len(data)),
		StreamSnapshotSourceOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.BorrowingEnabled() {
		t.Fatal("borrowing unexpectedly enabled by default")
	}

	borrowed, ok, err := source.BorrowAt(0, PageSize)
	if err != nil || ok || borrowed != nil {
		t.Fatalf("BorrowAt = len %d, ok %v, err %v", len(borrowed), ok, err)
	}
	if stream.borrowCalls != 0 {
		t.Fatalf("disabled source made %d BorrowAt calls", stream.borrowCalls)
	}
}

type borrowFallbackReader struct {
	data      []byte
	borrowErr error
	readCalls int
}

func (s *borrowFallbackReader) BorrowAt(uint64, uint64) ([]byte, bool, error) {
	return nil, false, s.borrowErr
}

func (s *borrowFallbackReader) ReadAt(buf []byte, _ uint64) (int, bool, error) {
	s.readCalls++
	copy(buf, s.data)
	return len(buf), false, nil
}

func TestReadFaultSourceBorrowErrorFallsBack(t *testing.T) {
	reader := &borrowFallbackReader{
		data:      bytes.Repeat([]byte{0x7c}, PageSize),
		borrowErr: errors.New("mmap unavailable"),
	}
	scratch := make([]byte, PageSize)
	copyBuf, n, zero, borrowed, borrowErr, err := readFaultSource(reader, scratch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(borrowErr, reader.borrowErr) {
		t.Fatalf("BorrowAt error = %v, want %v", borrowErr, reader.borrowErr)
	}
	if borrowed || zero || n != len(scratch) || reader.readCalls != 1 {
		t.Fatalf("fallback = n %d, zero %v, borrowed %v, reads %d", n, zero, borrowed, reader.readCalls)
	}
	if !bytes.Equal(copyBuf, reader.data) {
		t.Fatal("fallback data mismatch")
	}
}

func TestStreamSnapshotSourceBorrowErrorDisablesBorrowing(t *testing.T) {
	borrowErr := errors.New("mmap unavailable")
	data := bytes.Repeat([]byte{0x6d}, PageSize)
	stream := &borrowingTestStream{data: data, borrowErr: borrowErr}
	source, err := NewStreamSnapshotSource(
		context.Background(),
		stream,
		uint64(len(data)),
		StreamSnapshotSourceOptions{AllowFileBorrow: true},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := source.BorrowAt(0, PageSize); ok || !errors.Is(err, borrowErr) {
		t.Fatalf("first BorrowAt = ok %v, err %v", ok, err)
	}
	if source.BorrowingEnabled() {
		t.Fatal("borrowing remained enabled after a hard error")
	}
	if _, ok, err := source.BorrowAt(0, PageSize); ok || err != nil {
		t.Fatalf("disabled BorrowAt = ok %v, err %v", ok, err)
	}
	if stream.borrowCalls != 1 {
		t.Fatalf("BorrowAt calls = %d, want 1", stream.borrowCalls)
	}
}

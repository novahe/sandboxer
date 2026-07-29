package uffd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// StreamSnapshotSource implements SnapshotReader against a snapshot bundle's
// memory section, backed by any fetch.Stream — a local file (sparse-aware), a
// manifest (chunk-granular via cache-ctl), or an overlay of several (the
// incremental layered chain; see docs/sandbox.md §3.5). The memory section
// starts at bundle offset 0, sized RAMSize.
//
// Zero classification comes from Stream.RunAt: a Hole (merged across all
// layers — non-resident / declared hole everywhere) or an IsZero chunk needs
// no fetch and is served as a zero page; a Data run is fetched via
// Stream.ReadAt (from whichever layer holds it). The lazy-load ratio matches
// the underlying sparseness — no extra zero pages fetched, no hole pages
// over-served.
//
// Safe for concurrent use: the Stream is concurrent-safe and ctx is shared
// read-only.
type StreamSnapshotSource struct {
	stream         fetch.Stream
	ctx            context.Context
	ramSize        uint64
	borrower       borrowingStream
	borrowDisabled atomic.Bool
}

// StreamSnapshotSourceOptions controls optional source capabilities. The zero
// value preserves the buffered ReadAt path.
type StreamSnapshotSourceOptions struct {
	// AllowFileBorrow permits a stable local file mapping to be passed directly
	// to UFFDIO_COPY. The caller must guarantee that the mapped artifact remains
	// immutable and readable until the source is closed.
	AllowFileBorrow bool
}

type borrowingStream interface {
	BorrowAt(offset, limit uint64) (data []byte, ok bool, err error)
}

// NewStreamSnapshotSource binds a stream to the snapshot bundle and captures a
// parent context that scopes all subsequent ReadAt calls (the SnapshotReader
// interface is sync; cancellation goes through this stored ctx).
//
// ramSize is the memory section size (= sandbox memory capacity); it must be ≤
// stream.Size() because the bundle = memory + ZIP and the source only serves
// the memory prefix.
func NewStreamSnapshotSource(
	ctx context.Context,
	stream fetch.Stream,
	ramSize uint64,
	opts StreamSnapshotSourceOptions,
) (*StreamSnapshotSource, error) {
	if stream == nil {
		return nil, errors.New("uffd: nil stream")
	}
	if ramSize == 0 {
		return nil, errors.New("uffd: ramSize must be > 0")
	}
	if ramSize > stream.Size() {
		return nil, fmt.Errorf("uffd: ramSize %d exceeds bundle size %d", ramSize, stream.Size())
	}
	var borrower borrowingStream
	if opts.AllowFileBorrow {
		borrower, _ = stream.(borrowingStream)
	}
	return &StreamSnapshotSource{
		stream:   stream,
		ctx:      ctx,
		ramSize:  ramSize,
		borrower: borrower,
	}, nil
}

// BorrowingEnabled reports whether this source can currently use the optional
// local-file borrowing path. A hard BorrowAt error disables it for the
// remainder of the source lifetime; buffered ReadAt remains available.
func (s *StreamSnapshotSource) BorrowingEnabled() bool {
	return s.borrower != nil && !s.borrowDisabled.Load()
}

// ReadAt implements SnapshotReader.
//
// It classifies the run at memfdOffset via Stream.RunAt:
//   - memfdOffset ≥ ramSize → (0, true, io.EOF)
//   - a zero region (Hole or IsZero chunk) covering the whole first page →
//     (PageSize-aligned zero run, true, nil), no RPC issued
//   - otherwise → a data run, extended across contiguous data chunks up to
//     the next zero region or the buffer end, fetched via Stream.ReadAt and
//     returned (n, false, nil)
//
// A page straddling a zero→data boundary is served as data; Stream.ReadAt
// zero-fills the leading zero bytes, so real data is never served as zeros.
// The uploader's holes are page-aligned (memfd page granularity), so the
// straddle case only arises at end-of-image.
func (s *StreamSnapshotSource) ReadAt(buf []byte, memfdOffset uint64) (int, bool, error) {
	runBytes, isZero, err := s.classifyRun(memfdOffset, uint64(len(buf)))
	if err != nil {
		return 0, true, err
	}
	if isZero {
		return int(runBytes), true, nil
	}

	n, err := s.stream.ReadAt(s.ctx, buf[:runBytes], memfdOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, fmt.Errorf("snapshot ReadAt at %d: %w", memfdOffset, err)
	}
	// Zero-pad if the stream returned short (image ends inside this run).
	for i := n; i < int(runBytes); i++ {
		buf[i] = 0
	}
	return int(runBytes), false, nil
}

// BorrowAt exposes an explicitly enabled local tarstream's mmap-backed Data
// extent directly to the UFFD handler. The private structural interface keeps
// sandboxer buildable with accelerator versions that predate the optional
// capability; manifest and layered streams retain the buffered ReadAt path.
func (s *StreamSnapshotSource) BorrowAt(memfdOffset, maxBytes uint64) ([]byte, bool, error) {
	if !s.BorrowingEnabled() {
		return nil, false, nil
	}
	runBytes, isZero, err := s.classifyRun(memfdOffset, maxBytes)
	if err != nil {
		return nil, false, err
	}
	if isZero || runBytes == 0 || runBytes%PageSize != 0 {
		return nil, false, nil
	}
	data, ok, err := s.borrower.BorrowAt(memfdOffset, runBytes)
	if err != nil {
		// mmap/setup failures are not useful to retry on every fault. Report
		// the first one so the handler can make the fallback observable, then
		// keep serving through buffered ReadAt.
		if s.borrowDisabled.CompareAndSwap(false, true) {
			return nil, false, err
		}
		return nil, false, nil
	}
	if !ok {
		return nil, false, nil
	}
	if uint64(len(data)) != runBytes {
		// A physical span shorter than the logical run cannot be installed as
		// one page-aligned UFFDIO_COPY. Preserve correctness via ReadAt.
		return nil, false, nil
	}
	return data, true, nil
}

// classifyRun returns the zero/data classification and page-aligned logical
// length shared by ReadAt and BorrowAt.
func (s *StreamSnapshotSource) classifyRun(memfdOffset, maxBytes uint64) (uint64, bool, error) {
	if memfdOffset >= s.ramSize {
		return 0, true, io.EOF
	}
	avail := s.ramSize - memfdOffset
	if maxBytes > avail {
		maxBytes = avail
	}
	if maxBytes == 0 {
		return 0, true, nil
	}
	end := memfdOffset + maxBytes

	kind, runEnd, rerr := s.stream.RunAt(memfdOffset, end-memfdOffset)
	if rerr != nil { // io.EOF — guarded above (memfdOffset < ramSize ≤ Size)
		return 0, true, io.EOF
	}
	if kind != sparse.Data {
		// Zero region. Serve a zero page only when the whole first page (or
		// the rest of the image) is zero, so a page straddling zero→data is
		// never served as zeros.
		firstPageEnd := memfdOffset + PageSize
		if firstPageEnd > s.ramSize {
			firstPageEnd = s.ramSize
		}
		if runEnd >= firstPageEnd {
			zeroBytes := runEnd - memfdOffset
			if zeroBytes >= PageSize {
				zeroBytes -= zeroBytes % PageSize
			}
			return zeroBytes, true, nil
		}
		// Sub-page zero region not reaching the page end (mid-image straddle):
		// fall through and serve the page as data.
	} else {
		// Extend the data run across contiguous data chunks (RunAt caps each
		// Data run at one chunk) up to the next zero region or the buffer end,
		// so one fault fetches a whole data span concurrently.
		for runEnd < end {
			k2, e2, e2err := s.stream.RunAt(runEnd, end-runEnd)
			if e2err != nil || k2 != sparse.Data {
				break
			}
			runEnd = e2
		}
	}

	// Data run. Round down to a page boundary; a sub-page run only occurs at
	// end-of-image, where we serve the (≤ one page) remainder.
	runBytes := runEnd - memfdOffset
	if runBytes >= PageSize {
		runBytes -= runBytes % PageSize
	} else {
		runBytes = maxBytes
	}
	return runBytes, false, nil
}

var _ SnapshotBorrower = (*StreamSnapshotSource)(nil)

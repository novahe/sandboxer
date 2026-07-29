package uffd

// SnapshotReader supplies the page contents the handler installs at a
// given memfd offset on an Absent fault.
//
// Single-call protocol: ReadAt returns both the run classification
// (zero vs data) AND the run length the source can serve in one
// call, page-aligned and bounded by len(buf). The source picks the
// length to align with its own internal boundaries — chunk edges for
// manifest-backed streams, hole/data boundaries for file-backed
// streams. Handler must accept any returned n; pages beyond will be
// served by separate calls when faulted.
//
// Implementations:
//   - ZeroSource:           cold-start. Always zero, full buf.
//   - StreamSnapshotSource: restore. Wraps a fetch.Stream (local file,
//     manifest, or layered overlay); merged holes →
//     zero, data runs fetched from the serving layer.
//
// Implementations must be safe for concurrent calls.
type SnapshotReader interface {
	// ReadAt fills buf with up to len(buf) bytes starting at memfdOffset
	// and returns the run's classification.
	//
	// Return contract:
	//
	//	n:    page-aligned bytes covered (PageSize ≤ n ≤ len(buf), or 0 at EOF)
	//	zero: true  → source did NOT write to buf; handler installs zero pages
	//	      false → buf[:n] holds plaintext; handler copies them
	//	err:  io.EOF when memfdOffset ≥ source size (n=0); transport otherwise.
	//
	// memfdOffset and len(buf) are guaranteed PageSize-aligned by the
	// handler. Implementations must round n down to a PageSize multiple
	// (zero-padding the partial last page internally on file/network
	// short reads).
	//
	// Safe for concurrent use.
	ReadAt(buf []byte, memfdOffset uint64) (n int, zero bool, err error)
}

// SnapshotBorrower is an optional zero-copy-source companion to
// SnapshotReader. BorrowAt returns a stable read-only Data view that the
// handler may pass directly to UFFDIO_COPY, avoiding the intermediate worker
// buffer. The view must remain valid until the SnapshotReader's owner closes
// it.
//
// data[0] must correspond exactly to memfdOffset. This keeps the faulting page
// at the start of the borrowed run, which the handler's single-page retry
// relies on after a partial batched UFFDIO_COPY.
//
// ok is false when the source cannot safely borrow this range; the handler
// falls back to SnapshotReader.ReadAt. Successful data must be page-aligned in
// length and bounded by maxBytes.
type SnapshotBorrower interface {
	BorrowAt(memfdOffset, maxBytes uint64) (data []byte, ok bool, err error)
}

// ZeroSource implements SnapshotReader for cold-start. Every page is
// zero; the source never writes to buf.
type ZeroSource struct{}

func (ZeroSource) ReadAt(buf []byte, _ uint64) (int, bool, error) {
	return len(buf), true, nil
}

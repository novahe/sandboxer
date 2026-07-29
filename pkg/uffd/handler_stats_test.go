package uffd

import "testing"

func TestHandlerStatsBorrowedCopyAccounting(t *testing.T) {
	h := &Handler{}
	h.stats.borrowedCopies.Store(2)
	h.stats.borrowedPartial.Store(1)
	h.stats.borrowedFallbackErrors.Store(1)
	h.stats.pagesBorrowed.Store(3)

	stats := h.Stats()
	for key, want := range map[string]uint64{
		"borrowed_copy_calls":      2,
		"borrowed_partial_batches": 1,
		"borrowed_fallback_errors": 1,
		"pages_borrowed_confirmed": 3,
	} {
		if got := stats[key]; got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	if _, exists := stats["pages_borrowed"]; exists {
		t.Fatal("ambiguous pages_borrowed metric is still exported")
	}
}

func TestRecordBorrowedCopyConfirmsOnlyInstalledPages(t *testing.T) {
	h := &Handler{}

	h.recordBorrowedCopy(4, false)
	h.recordBorrowedCopy(2, true)

	if got := h.stats.borrowedCopies.Load(); got != 2 {
		t.Fatalf("borrowed copy calls = %d, want 2", got)
	}
	if got := h.stats.pagesBorrowed.Load(); got != 2 {
		t.Fatalf("confirmed borrowed pages = %d, want 2", got)
	}
}

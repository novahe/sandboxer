package sandbox

import "testing"

func TestBuildUffdJSONBorrowedCopyAccounting(t *testing.T) {
	got := buildUffdJSON(map[string]uint64{
		"borrowed_copy_calls":      2,
		"borrowed_partial_batches": 1,
		"borrowed_fallback_errors": 1,
		"pages_borrowed_confirmed": 3,
	}, 4*4096)

	if got.BorrowedCopyCalls != 2 {
		t.Errorf("BorrowedCopyCalls = %d, want 2", got.BorrowedCopyCalls)
	}
	if got.BorrowedPartialBatches != 1 {
		t.Errorf("BorrowedPartialBatches = %d, want 1", got.BorrowedPartialBatches)
	}
	if got.BorrowedFallbackErrors != 1 {
		t.Errorf("BorrowedFallbackErrors = %d, want 1", got.BorrowedFallbackErrors)
	}
	if got.PagesBorrowedConfirmed != 3 {
		t.Errorf("PagesBorrowedConfirmed = %d, want 3", got.PagesBorrowedConfirmed)
	}
}

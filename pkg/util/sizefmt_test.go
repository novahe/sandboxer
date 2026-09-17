package util

import (
	"math"
	"testing"
)

func TestFormatMiB(t *testing.T) {
	for _, test := range []struct {
		name  string
		bytes uint64
		want  string
	}{
		{"zero", 0, "0.0MiB"},
		{"one_byte", 1, "0.0MiB"},
		{"below_first_rounding_boundary", 52428, "0.0MiB"},
		{"above_first_rounding_boundary", 52429, "0.1MiB"},
		{"half_mib", 1 << 19, "0.5MiB"},
		{"below_mib", (1 << 20) - 1, "1.0MiB"},
		{"exact_mib", 1 << 20, "1.0MiB"},
		{"above_mib", (1 << 20) + 1, "1.0MiB"},
		{"below_tie", (5 << 18) - 1, "1.2MiB"},
		{"tie_rounds_down_to_even", 5 << 18, "1.2MiB"},
		{"above_tie", (5 << 18) + 1, "1.3MiB"},
		{"tie_rounds_up_to_even", 7 << 18, "1.8MiB"},
		{"gib_stays_in_mib", 1 << 30, "1024.0MiB"},
		{"tib_stays_in_mib", 1 << 40, "1048576.0MiB"},
		{"max_uint64", math.MaxUint64, "17592186044416.0MiB"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := FormatMiB(test.bytes); got != test.want {
				t.Errorf("FormatMiB(%d) = %q, want %q", test.bytes, got, test.want)
			}
		})
	}
}

func TestParseSizeExactDecimal(t *testing.T) {
	for _, test := range []struct {
		input string
		want  uint64
	}{
		{"4096", 4096},
		{"0.5KiB", 512},
		{"1.5GiB", 3 << 29},
		{"536875008B", 512<<20 + 4096},
		{"18446744073709551615", math.MaxUint64},
	} {
		got, err := ParseSize(test.input)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("ParseSize(%q)=%d, want %d", test.input, got, test.want)
		}
	}
}

func TestParseSizeRejectsInexactAndOverflow(t *testing.T) {
	for _, input := range []string{
		"0.1B",
		"1.0000000000000000001B",
		"18446744073709551616",
		"1e3",
		"NaN",
		"Inf",
		"-1GiB",
		"1..0GiB",
	} {
		if got, err := ParseSize(input); err == nil {
			t.Fatalf("ParseSize(%q)=%d, want error", input, got)
		}
	}
}

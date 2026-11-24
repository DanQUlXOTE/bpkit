package ui

import "strings"

import "testing"

func TestWrapKeepsWithinWidth(t *testing.T) {
	// A real Bindplane error: one long line with embedded newlines and tabs.
	err := `bindplane POST /v1/apply: 400 {"errors":["failed to render config ` +
		`(resourceID: ): 3 errors occurred:\n\t* unknown SourceType: tcp:9\n\t* ` +
		`unknown SourceType: file_v2:19\n\n"]}`
	for _, width := range []int{40, 60, 80, 120} {
		got := Wrap(err, width)
		for _, line := range strings.Split(got, "\n") {
			if n := len([]rune(line)); n > width {
				t.Fatalf("width %d: line of %d runes: %q", width, n, line)
			}
		}
		// Nothing may be lost in the reflow.
		flat := strings.Join(strings.Fields(strings.ReplaceAll(got, "\n", " ")), "")
		want := strings.Join(strings.Fields(strings.ReplaceAll(strings.ReplaceAll(err, "\t", " "), "\n", " ")), "")
		if flat != want {
			t.Fatalf("width %d: content changed\n got %q\nwant %q", width, flat, want)
		}
	}
}

func TestWrapHardBreaksLongTokens(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := Wrap(long, 40)
	for _, line := range strings.Split(got, "\n") {
		if n := len([]rune(line)); n > 40 {
			t.Fatalf("line of %d runes: %q", n, line)
		}
	}
	if n := strings.Count(strings.ReplaceAll(got, "\n", ""), "x"); n != 200 {
		t.Fatalf("lost characters: %d of 200", n)
	}
}

func TestWrapPreservesLineBreaks(t *testing.T) {
	got := Wrap("first line\n\nsecond line", 40)
	if got != "first line\nsecond line" {
		t.Fatalf("got %q", got)
	}
}

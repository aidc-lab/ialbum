package storage

import "testing"

func TestNormalizeRelative(t *testing.T) {
	t.Parallel()
	valid := map[string]string{"": "", ".": "", "照片/旅行.jpg": "照片/旅行.jpg", `foo\bar.jpg`: "foo/bar.jpg", "a/../b.jpg": "b.jpg"}
	for input, want := range valid {
		got, err := NormalizeRelative(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeRelative(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"/etc/passwd", "../secret", "a/../../secret", "bad\x00name"} {
		if _, err := NormalizeRelative(input); err == nil {
			t.Fatalf("NormalizeRelative(%q) should fail", input)
		}
	}
}

package vfs

import "testing"

func TestWinHostPath(t *testing.T) {
	ok := map[string]string{"/C:": `C:\`, "/c:/": `C:\`, "/C:/Users/x": `C:\Users\x`}
	for in, want := range ok {
		if got, err := winHostPath(in); err != nil || got != want {
			t.Errorf("winHostPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/C:x", "/C:/a/..\\..\\b", "/C:/a:stream", "/C:/con", "/C:/dir/NUL", "//server/share", "/1:/x"} {
		if got, err := winHostPath(bad); err == nil {
			t.Errorf("winHostPath(%q) = %q, want an error", bad, got)
		}
	}
}

package pathutil

import "testing"

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		p, root string
		want    bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/x/y", "/a/b", false},
		{"/a/b/..c", "/a/b", true}, // a file named "..c", not a parent
	} {
		if got := Within(tc.p, tc.root); got != tc.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tc.p, tc.root, got, tc.want)
		}
	}
}

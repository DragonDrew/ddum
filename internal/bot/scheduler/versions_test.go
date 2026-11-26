package scheduler

import "testing"

func TestReleaseVersion(t *testing.T) {
	cases := map[string]bool{
		"0.11.0":                  true,
		"v0.11.0":                 true,
		"0.11.0-3-gfeacd88-dirty": true, // git describe after a tag: still a release line
		"dev":                     false,
		"":                        false,
		"feacd88":                 false, // git describe --always with no tag
		"7654321":                 false, // the same, when the hash is all digits
		"release-0.11.0":          false,
	}
	for v, want := range cases {
		if got := releaseVersion(v); got != want {
			t.Errorf("releaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

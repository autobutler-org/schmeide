package release

import "testing"

func TestSkewed(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.4", true},
		{"dev", "v1.2.3", false},
		{"v1.2.3", "abc1234", false},
		{"dev", "dev", false},
	} {
		if got := Skewed(c.a, c.b); got != c.want {
			t.Errorf("Skewed(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

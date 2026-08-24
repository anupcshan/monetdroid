package monetdroid

import (
	"testing"
)

func TestFormatLineContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"code line", "fmt.Println(name)", "fmt.Println(name)"},
		{"blank line", "", `""`},
		{"whitespace only", "   ", `"   "`},
	}
	for _, tc := range cases {
		if got := formatLineContent(tc.content); got != tc.want {
			t.Errorf("%s: formatLineContent(%q) = %q, want %q", tc.name, tc.content, got, tc.want)
		}
	}
}

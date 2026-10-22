//go:build linux

package inputbridge

import (
	"slices"
	"strings"
	"testing"
)

// Only mount points strictly under dir count, unescaped (XERK-2178).
func TestParseMountsUnder(t *testing.T) {
	const info = `76 1 0:40 / /var rw - xfs /dev/sda rw
3749 76 0:594 / /sessions/4673be1c/runtime rw,nosuid,nodev,relatime shared:543 - tmpfs none rw
3750 76 0:5 /input/event3 /sessions/4673be1c/input/event3 rw shared:1 - devtmpfs dev rw
3751 76 0:5 / /sessions/4673be1c/a\040b rw - tmpfs none rw
3752 76 0:5 / /sessions/4673be1c rw - tmpfs none rw
3753 76 0:5 / /sessions/4673be1c2/runtime rw - tmpfs none rw
`
	got, err := parseMountsUnder(strings.NewReader(info), "/sessions/4673be1c")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/sessions/4673be1c/runtime", "/sessions/4673be1c/input/event3", "/sessions/4673be1c/a b"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

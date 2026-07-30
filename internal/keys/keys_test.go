package keys

import (
	"strings"
	"testing"
)

func TestGenerateShape(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		raw, hash, display, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if !strings.HasPrefix(raw, Prefix) {
			t.Fatalf("missing prefix: %q", raw)
		}
		if got := len(raw) - len(Prefix); got != bodyLen {
			t.Fatalf("body length = %d, want %d", got, bodyLen)
		}
		for _, c := range raw[len(Prefix):] {
			if !strings.ContainsRune(alphabet, c) {
				t.Fatalf("character %q outside alphabet in %q", c, raw)
			}
		}
		if len(hash) != 32 {
			t.Fatalf("hash length = %d, want 32", len(hash))
		}
		sum := Hash(raw)
		if !Equal(hash, sum[:]) {
			t.Fatal("returned hash does not match Hash of the raw key")
		}
		if seen[raw] {
			t.Fatalf("duplicate key generated: %q", raw)
		}
		seen[raw] = true

		if !strings.HasPrefix(display, Prefix) || !strings.Contains(display, "...") {
			t.Fatalf("display form is wrong: %q", display)
		}
		if strings.Contains(display, raw[len(Prefix)+8:len(Prefix)+12]) {
			t.Fatalf("display leaks the middle of the key: %q", display)
		}
	}
}

func TestDisplayShort(t *testing.T) {
	if got := Display("qp_abc"); got != "qp_..." {
		t.Fatalf("Display(short) = %q", got)
	}
}

func TestFromAuthHeader(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Bearer qp_abc", "qp_abc", true},
		{"bearer qp_abc", "qp_abc", true},
		{"BEARER  qp_abc  ", "qp_abc", true},
		{"Basic qp_abc", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"", "", false},
		{"qp_abc", "", false},
	}
	for _, c := range cases {
		got, ok := FromAuthHeader(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("FromAuthHeader(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func BenchmarkHashAndCompare(b *testing.B) {
	raw, hash, _, _ := Generate()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sum := Hash(raw)
		if !Equal(sum[:], hash) {
			b.Fatal("mismatch")
		}
	}
}

package web

import "testing"

func TestMoney(t *testing.T) {
	for in, want := range map[string]string{"0": "0.00", "25.5": "25.50", "1530": "1,530.00", "135203907": "135,203,907.00", "999.123456": "999.123456"} {
		if got := money(in); got != want {
			t.Errorf("money(%q) = %q, want %q", in, got, want)
		}
	}
}

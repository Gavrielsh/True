package main

import "testing"

func TestAddr(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"8080", ":8080"},
		{":9000", ":9000"},
		{"0.0.0.0:8080", "0.0.0.0:8080"},
		{"127.0.0.1:5000", "127.0.0.1:5000"},
	}
	for _, tc := range cases {
		if got := addr(tc.in); got != tc.want {
			t.Errorf("addr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

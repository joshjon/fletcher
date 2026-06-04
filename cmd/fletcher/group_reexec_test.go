package main

import "testing"

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"doctor", "doctor"},
		{"--check-latest", "--check-latest"},
		{"/usr/local/bin/fletcher", "/usr/local/bin/fletcher"},
		{"host:51820", "host:51820"},
		{"KEY=val", "KEY=val"},
		{"", "''"},
		{"two words", "'two words'"},
		{"semi;colon", "'semi;colon'"},
		{"it's", `'it'\''s'`},
		{"$(touch pwn)", "'$(touch pwn)'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"/usr/local/bin/fletcher", "peer", "add", "my phone"})
	want := "/usr/local/bin/fletcher peer add 'my phone'"
	if got != want {
		t.Errorf("shellJoin = %q, want %q", got, want)
	}
}

func TestHasServeCommand(t *testing.T) {
	if !hasServeCommand([]string{"serve"}) {
		t.Error("expected serve to be detected")
	}
	if hasServeCommand([]string{"doctor"}) {
		t.Error("doctor must not be detected as serve")
	}
	if hasServeCommand(nil) {
		t.Error("empty args must not be detected as serve")
	}
}

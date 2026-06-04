package main

import "testing"

func TestDefaultImageName(t *testing.T) {
	cases := map[string]string{
		"fletcher-base:dev":              "fletcher-base",
		"fletcher-base":                  "fletcher-base",
		"registry.example.com/foo/bar:1": "bar",
		"foo/bar":                        "bar",
		"ghcr.io/org/img@sha256:abc123":  "img",
	}
	for in, want := range cases {
		if got := defaultImageName(in); got != want {
			t.Errorf("defaultImageName(%q) = %q, want %q", in, got, want)
		}
	}
}

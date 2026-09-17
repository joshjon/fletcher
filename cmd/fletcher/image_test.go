package main

import (
	"testing"

	"github.com/joshjon/fletcher/internal/image"
)

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

func TestShouldPullBeforeUpdate(t *testing.T) {
	cases := map[string]struct {
		meta image.TemplateMeta
		want bool
	}{
		"local-only image": {
			meta: image.TemplateMeta{Source: "fletcher-base:dev"},
			want: false,
		},
		"registry image": {
			meta: image.TemplateMeta{Source: "ghcr.io/joshjon/fletcher-base:debian-13", Digest: "sha256:abc123"},
			want: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := shouldPullBeforeUpdate(tc.meta); got != tc.want {
				t.Fatalf("shouldPullBeforeUpdate() = %v, want %v", got, tc.want)
			}
		})
	}
}

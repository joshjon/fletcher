//go:build linux

package main

import (
	"reflect"
	"testing"
)

func TestMergeEnv(t *testing.T) {
	tests := []struct {
		name     string
		base     []string
		override []string
		want     []string
	}{
		{
			name:     "no override returns base unchanged",
			base:     []string{"A=1", "B=2"},
			override: nil,
			want:     []string{"A=1", "B=2"},
		},
		{
			name:     "override replaces a shared key and keeps base-only keys",
			base:     []string{"A=image", "B=image"},
			override: []string{"A=user"},
			want:     []string{"B=image", "A=user"},
		},
		{
			name:     "override adds a new key",
			base:     []string{"A=1"},
			override: []string{"NEW=2"},
			want:     []string{"A=1", "NEW=2"},
		},
		{
			name:     "empty base yields the override",
			base:     nil,
			override: []string{"A=1"},
			want:     []string{"A=1"},
		},
		{
			name:     "value containing = is preserved, key matched on first =",
			base:     []string{"URL=old"},
			override: []string{"URL=https://x/?a=b"},
			want:     []string{"URL=https://x/?a=b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeEnv(tt.base, tt.override)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("mergeEnv(%q, %q) = %q, want %q", tt.base, tt.override, got, tt.want)
			}
		})
	}
}

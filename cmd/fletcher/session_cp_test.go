package main

import "testing"

func TestSplitRemote(t *testing.T) {
	tests := []struct {
		arg        string
		wantRef    string
		wantPath   string
		wantRemote bool
	}{
		{"mybox:/home/fletcher/x", "mybox", "/home/fletcher/x", true},
		{"sess_01:data.csv", "sess_01", "data.csv", true},
		{"mybox:", "mybox", "", true},
		{"./local.txt", "", "./local.txt", false},
		{"/etc/hosts", "", "/etc/hosts", false},
		{"local.txt", "", "local.txt", false},
		{"./weird:name", "", "./weird:name", false}, // leading ./ forces local
		{":nope", "", ":nope", false},               // empty ref is not remote
		{"a/b:c", "", "a/b:c", false},               // slash before colon is a local path
	}
	for _, tt := range tests {
		ref, p, remote := splitRemote(tt.arg)
		if ref != tt.wantRef || p != tt.wantPath || remote != tt.wantRemote {
			t.Errorf("splitRemote(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.arg, ref, p, remote, tt.wantRef, tt.wantPath, tt.wantRemote)
		}
	}
}

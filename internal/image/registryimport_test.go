package image

import "testing"

func TestDefaultName(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/joshjon/app:v1":  "app",
		"nginx:alpine":            "nginx",
		"docker.io/library/redis": "redis",
		"app":                     "app",
	}
	for ref, want := range cases {
		if got := DefaultName(ref); got != want {
			t.Errorf("DefaultName(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestLowestExposedPort(t *testing.T) {
	if got := lowestExposedPort(map[string]struct{}{"443/tcp": {}, "80/tcp": {}}); got != 80 {
		t.Errorf("lowest of {443,80} = %d, want 80", got)
	}
	if got := lowestExposedPort(map[string]struct{}{"8080/tcp": {}}); got != 8080 {
		t.Errorf("single port = %d, want 8080", got)
	}
	if got := lowestExposedPort(nil); got != 0 {
		t.Errorf("no ports = %d, want 0", got)
	}
}

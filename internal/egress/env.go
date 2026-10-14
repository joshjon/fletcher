package egress

import "strings"

// EnvForPolicy strips proxy variables when the host supplies no egress forward.
func EnvForPolicy(policy string, env []string) []string {
	if Normalize(policy) != PolicyNone {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY":
		default:
			out = append(out, kv)
		}
	}
	return out
}

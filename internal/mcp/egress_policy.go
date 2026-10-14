package mcp

import (
	"errors"
	"net/http"

	"github.com/joshjon/fletcher/internal/egress"
	"github.com/joshjon/fletcher/internal/forkauth"
)

type policyTransport struct {
	next      http.RoundTripper
	allowlist egress.Allowlist
}

func (t policyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.check(r); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(r)
}

func (t policyTransport) check(r *http.Request) error {
	caller, ok := forkauth.FromContext(r.Context())
	if !ok {
		return errors.New("authenticated fork identity required for egress")
	}
	switch caller.Policy {
	case egress.PolicyOpen:
		return nil
	case egress.PolicyAllowlist:
		if t.allowlist.Allow(r.URL.Hostname()) {
			return nil
		}
	}
	return errors.New("egress denied by fork policy")
}

func policyHTTPClient(client *http.Client, allowlist egress.Allowlist) *http.Client {
	copyClient := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	// Every RoundTrip is checked, including redirects followed by http.Client.
	copyClient.Transport = policyTransport{next: transport, allowlist: allowlist}
	return &copyClient
}

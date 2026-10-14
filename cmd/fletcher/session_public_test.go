package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

func TestExternalTLSPortOutput(t *testing.T) {
	port := &fletcherv1.PublishedPort{Name: "app", Public: true, Host: "app.example.com", GuestPort: 8080, TlsStatus: "external"}
	var out bytes.Buffer
	require.NoError(t, renderPublishedPort(t.Context(), &out, "", port, "", "192.0.2.1"))
	require.Contains(t, out.String(), "externally managed, not verified")
	require.NotContains(t, out.String(), "create this DNS record")
	require.NotContains(t, out.String(), "192.0.2.1")
	require.NotContains(t, out.String(), "cert issues")
	out.Reset()
	require.NoError(t, writePublishedPortsTable(t.Context(), &out, []*fletcherv1.PublishedPort{port}, "", "192.0.2.1"))
	require.Contains(t, out.String(), "operator-managed")
	require.Contains(t, out.String(), "external (not verified)")
	out.Reset()
	require.NoError(t, renderPublishedPort(t.Context(), &out, "json", port, "", ""))
	require.Contains(t, out.String(), "external")
}

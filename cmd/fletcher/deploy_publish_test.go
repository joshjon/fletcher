package main

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

type publishFixture struct {
	port       *fletcherv1.PublishedPort
	published  []*fletcherv1.PublishedPort
	publishErr error
	listErr    error
	reads      int
	seenRef    string
	seenHost   string
}

func (f *publishFixture) PublishPort(_ context.Context, req *connect.Request[fletcherv1.PublishPortRequest]) (*connect.Response[fletcherv1.PublishPortResponse], error) {
	f.seenRef, f.seenHost = req.Msg.GetRef(), req.Msg.GetHost()
	if f.publishErr != nil {
		return nil, f.publishErr
	}
	return connect.NewResponse(&fletcherv1.PublishPortResponse{Port: f.port}), nil
}

func (f *publishFixture) ListPorts(_ context.Context, req *connect.Request[fletcherv1.ListPortsRequest]) (*connect.Response[fletcherv1.ListPortsResponse], error) {
	f.reads++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return connect.NewResponse(&fletcherv1.ListPortsResponse{Ports: f.published, PublicIp: "198.51.100.42"}), nil
}

func TestPublishDeploymentReconcilesOnlyMatchingImmutableIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		changed string
		found   bool
	}{
		{name: "lost response", found: true},
		{name: "different session", changed: "session"},
		{name: "different port", changed: "port"},
		{name: "different hostname", changed: "host"},
		{name: "different access", changed: "access"},
		{name: "missing result", changed: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			port := &fletcherv1.PublishedPort{Id: "published", SessionId: "session-owned", GuestPort: 8080, Public: true, Host: "app.example.com"}
			switch test.changed {
			case "session":
				port.SessionId = "session-other"
			case "port":
				port.GuestPort = 9090
			case "host":
				port.Host = "other.example.com"
			case "access":
				port.Public = false
			case "missing":
				port.Id = ""
			}
			uncertain := connect.NewError(connect.CodeUnavailable, nil)
			client := &publishFixture{publishErr: uncertain, published: []*fletcherv1.PublishedPort{port}}
			response, err := publishDeployment(t.Context(), client, &fletcherv1.PublishPortRequest{Ref: "session-owned", GuestPort: 8080, Public: true, Host: " APP.EXAMPLE.COM "})
			require.Equal(t, "session-owned", client.seenRef)
			require.Equal(t, "app.example.com", client.seenHost)
			require.Equal(t, 1, client.reads)
			if test.found {
				require.NoError(t, err)
				require.Equal(t, port, response.Msg.GetPort())
				require.Equal(t, "198.51.100.42", response.Msg.GetPublicIp())
			} else {
				require.ErrorIs(t, err, uncertain)
			}
		})
	}
}

func TestPublishDeploymentPreservesBothPublishAndInspectionErrors(t *testing.T) {
	publishErr := connect.NewError(connect.CodeDeadlineExceeded, nil)
	readErr := connect.NewError(connect.CodeUnavailable, nil)
	client := &publishFixture{publishErr: publishErr, listErr: readErr}
	_, err := publishDeployment(t.Context(), client, &fletcherv1.PublishPortRequest{Ref: "session-owned", GuestPort: 8080})
	require.ErrorIs(t, err, publishErr)
	require.ErrorIs(t, err, readErr)
}

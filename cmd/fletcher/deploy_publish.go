package main

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

// deployPublisher deliberately excludes delete: an uncertain publish must not
// destroy a disk that may already be running or publicly reachable.
type deployPublisher interface {
	PublishPort(context.Context, *connect.Request[fletcherv1.PublishPortRequest]) (*connect.Response[fletcherv1.PublishPortResponse], error)
	ListPorts(context.Context, *connect.Request[fletcherv1.ListPortsRequest]) (*connect.Response[fletcherv1.ListPortsResponse], error)
}

func publishDeployment(ctx context.Context, client deployPublisher, req *fletcherv1.PublishPortRequest) (*connect.Response[fletcherv1.PublishPortResponse], error) {
	req.Host = strings.ToLower(strings.TrimSpace(req.GetHost()))
	response, err := client.PublishPort(ctx, connect.NewRequest(req))
	if err == nil && response != nil && deploymentPortMatches(response.Msg.GetPort(), req) {
		return response, nil
	}
	if err == nil {
		err = errors.New("publish response did not identify the requested exposure")
	}
	ports, readErr := client.ListPorts(ctx, connect.NewRequest(&fletcherv1.ListPortsRequest{Ref: req.GetRef()}))
	if readErr != nil {
		return nil, errors.Join(err, readErr)
	}
	for _, port := range ports.Msg.GetPorts() {
		if deploymentPortMatches(port, req) {
			return connect.NewResponse(&fletcherv1.PublishPortResponse{Port: port, PublicIp: ports.Msg.GetPublicIp()}), nil
		}
	}
	return nil, err
}

func deploymentPortMatches(port *fletcherv1.PublishedPort, request *fletcherv1.PublishPortRequest) bool {
	return port.GetId() != "" && port.GetSessionId() == request.GetRef() &&
		port.GetGuestPort() == request.GetGuestPort() && port.GetPublic() == request.GetPublic() &&
		port.GetHost() == request.GetHost()
}

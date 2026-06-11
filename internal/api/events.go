package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/events"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// EventsService implements fletcherv1connect.EventServiceHandler: it streams
// the daemon's in-process event bus to clients so they can update live
// instead of polling.
type EventsService struct {
	fletcherv1connect.UnimplementedEventServiceHandler
	bus *events.Bus
}

// NewEventsService wires the service to the daemon's event bus.
func NewEventsService(bus *events.Bus) *EventsService {
	return &EventsService{bus: bus}
}

// WatchEvents streams events until the client disconnects.
func (s *EventsService) WatchEvents(ctx context.Context, _ *connect.Request[fletcherv1.WatchEventsRequest], stream *connect.ServerStream[fletcherv1.WatchEventsResponse]) error {
	ch, cancel := s.bus.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-ch:
			if err := stream.Send(&fletcherv1.WatchEventsResponse{
				Type:   e.Type,
				Action: e.Action,
				Id:     e.ID,
				Name:   e.Name,
				At:     e.At.Unix(),
			}); err != nil {
				// The client went away; disconnects are how streams end.
				return nil
			}
		}
	}
}

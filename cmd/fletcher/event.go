package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func eventCmd() *cli.Command {
	return &cli.Command{
		Name:  "event",
		Usage: "watch daemon lifecycle events",
		Commands: []*cli.Command{
			eventWatchCmd(),
		},
	}
}

func eventWatchCmd() *cli.Command {
	return &cli.Command{
		Name:  "watch",
		Usage: "stream lifecycle events (sessions, jobs, approvals, images) until interrupted",
		Flags: []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Events stream over the same h2c transport the shell uses.
			client := newEventsClient(cmd)
			stream, err := client.WatchEvents(ctx, connect.NewRequest(&fletcherv1.WatchEventsRequest{}))
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "watching events (Ctrl-C to stop)")
			for stream.Receive() {
				e := stream.Msg()
				name := e.GetName()
				if name == "" {
					name = e.GetId()
				}
				fmt.Printf("%s  %-9s %-10s %s\n",
					time.Unix(e.GetAt(), 0).Format(time.TimeOnly), e.GetType(), e.GetAction(), name)
			}
			// Ctrl-C ends the watch normally; only a stream failure while the
			// context was still live is an error.
			if err := stream.Err(); err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		},
	}
}

func newEventsClient(cmd *cli.Command) fletcherv1connect.EventServiceClient {
	if remote, token := resolveRemote(cmd); remote != "" {
		warnIfRemoteUnauthed(remote, token)
		hc := h2cClient("tcp", remote)
		return fletcherv1connect.NewEventServiceClient(hc, "http://"+remote,
			connect.WithInterceptors(bearerInterceptor{token: token}))
	}
	hc := h2cClient("unix", cmd.String("socket"))
	return fletcherv1connect.NewEventServiceClient(hc, unixBaseURL)
}

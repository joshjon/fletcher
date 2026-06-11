package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

func volumeCmd() *cli.Command {
	return &cli.Command{
		Name:  "volume",
		Usage: "create, inspect, and manage persistent volumes (disks that outlive sessions)",
		Commands: []*cli.Command{
			volumeCreateCmd(),
			volumeGetCmd(),
			volumeListCmd(),
			volumeDeleteCmd(),
		},
	}
}

func newVolumesClient(cmd *cli.Command) fletcherv1connect.VolumeServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewVolumeServiceClient(hc, base, opts...)
}

func volumeCreateCmd() *cli.Command {
	return &cli.Command{
		Name:  "create",
		Usage: "provision a new blank volume (attach it with `session create --volume`)",
		Flags: []cli.Flag{
			socketFlag(),
			outputFlag(),
			&cli.StringFlag{Name: "name", Usage: "unique volume name", Required: true},
			&cli.IntFlag{Name: "size-gb", Usage: "provisioned capacity in GiB; sparse, so disk use grows with data (default 10)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			resp, err := newVolumesClient(cmd).CreateVolume(ctx, connect.NewRequest(&fletcherv1.CreateVolumeRequest{
				Name:      cmd.String("name"),
				SizeBytes: int64(cmd.Int("size-gb")) << 30,
			}))
			if err != nil {
				return err
			}
			return renderVolume(cmd.String("output"), resp.Msg.GetVolume())
		},
	}
}

func volumeGetCmd() *cli.Command {
	return &cli.Command{
		Name:      "get",
		Usage:     "show a volume",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("volume ref (id or name) is required")
			}
			resp, err := newVolumesClient(cmd).GetVolume(ctx, connect.NewRequest(&fletcherv1.GetVolumeRequest{Ref: ref}))
			if err != nil {
				return err
			}
			return renderVolume(cmd.String("output"), resp.Msg.GetVolume())
		},
	}
}

func volumeListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list volumes",
		Flags: []cli.Flag{socketFlag(), outputFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			resp, err := newVolumesClient(cmd).ListVolumes(ctx, connect.NewRequest(&fletcherv1.ListVolumesRequest{}))
			if err != nil {
				return err
			}
			if cmd.String("output") == "json" {
				return writeProtoJSON(os.Stdout, resp.Msg)
			}
			volumes := resp.Msg.GetVolumes()
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tSIZE\tUSED\tATTACHED TO\tCREATED")
			for _, v := range volumes {
				attached := v.GetAttachedSession()
				if attached == "" {
					attached = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					v.GetId(), v.GetName(),
					humanBytes(v.GetSizeBytes()), humanBytes(v.GetUsedBytes()),
					attached, formatUnix(v.GetCreatedAt()))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Printf("\ntotal: %d\n", len(volumes))
			return nil
		},
	}
}

func volumeDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Usage:     "destroy a volume and its data (refused while attached to a session)",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("volume ref (id or name) is required")
			}
			if _, err := newVolumesClient(cmd).DeleteVolume(ctx, connect.NewRequest(&fletcherv1.DeleteVolumeRequest{Ref: ref})); err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", ref)
			return nil
		},
	}
}

func renderVolume(format string, v *fletcherv1.Volume) error {
	if format == "json" {
		return writeProtoJSON(os.Stdout, v)
	}
	attached := v.GetAttachedSession()
	if attached == "" {
		attached = "-"
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "id:\t%s\n", v.GetId())
	fmt.Fprintf(tw, "name:\t%s\n", v.GetName())
	fmt.Fprintf(tw, "size:\t%s\n", humanBytes(v.GetSizeBytes()))
	fmt.Fprintf(tw, "used:\t%s\n", humanBytes(v.GetUsedBytes()))
	fmt.Fprintf(tw, "attached:\t%s\n", attached)
	fmt.Fprintf(tw, "created_at:\t%s\n", formatUnix(v.GetCreatedAt()))
	return tw.Flush()
}

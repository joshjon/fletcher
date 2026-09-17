package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/settings"
)

func remoteImageUpdate(ctx context.Context, cmd *cli.Command) error {
	name := cmd.Args().First()
	if name == "" {
		var err error
		name, err = settingValue(ctx, cmd, settings.KeyDefaultImage)
		if err != nil {
			return err
		}
	}
	if name == "" {
		return errors.New("an image name or default_image setting is required")
	}
	if err := confirmManagement(cmd, "Update the image template? Existing session forks are unchanged."); err != nil {
		return err
	}
	r, err := newImageClient(cmd).StartImport(ctx, connect.NewRequest(&fletcherv1.StartImportRequest{RequestId: rand.Text(), Name: name, Update: true}))
	if err != nil {
		return err
	}
	return finishImageImport(ctx, cmd, r.Msg)
}

func finishImageImport(ctx context.Context, cmd *cli.Command, response *fletcherv1.StartImportResponse) error {
	if cmd.Bool("detach") {
		return renderManagement(cmd.String("output"), response)
	}
	op := response.GetOperation()
	fmt.Fprintf(os.Stderr, "import %s accepted; waiting (it continues on the daemon if this client disconnects)\n", op.GetId())
	for op.GetState() == "running" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		result, err := newImageClient(cmd).ListImports(ctx, connect.NewRequest(&fletcherv1.ListImportsRequest{}))
		if err != nil {
			return fmt.Errorf("inspect import %s with 'fletcher image imports': %w", op.GetId(), err)
		}
		found := false
		for _, current := range result.Msg.GetOperations() {
			if current.GetId() == op.GetId() {
				op = current
				found = true
				break
			}
		}
		if !found {
			return errors.New("import no longer in recent history; inspect the image before retrying")
		}
	}
	response.Operation = op
	if op.GetState() != "succeeded" {
		return fmt.Errorf("import %s: %s", op.GetId(), op.GetError())
	}
	return renderManagement(cmd.String("output"), response)
}

func imageImportsCmd() *cli.Command {
	return &cli.Command{Name: "imports", Usage: "show recent registry imports and updates", Flags: []cli.Flag{socketFlag(), outputFlag()}, Action: func(ctx context.Context, cmd *cli.Command) error {
		r, err := newImageClient(cmd).ListImports(ctx, connect.NewRequest(&fletcherv1.ListImportsRequest{}))
		if err != nil {
			return err
		}
		return renderManagement(cmd.String("output"), r.Msg)
	}}
}

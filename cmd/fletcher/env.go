package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
)

// envVarFlags are the shared --env / --secret flags for `session create` and
// `deploy`. A plain var carries its value; a secret var references a stored
// secret by name (managed with `fletcher secret`).
func envVarFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{Name: "env", Usage: "plain environment variable as KEY=VALUE (repeatable)"},
		&cli.StringSliceFlag{Name: "secret", Usage: "secret environment variable as KEY=SECRET_NAME, referencing a stored secret (repeatable; manage with `fletcher secret`)"},
	}
}

// parseEnvVarFlags turns the --env / --secret flags into proto env vars.
func parseEnvVarFlags(cmd *cli.Command) ([]*fletcherv1.EnvVar, error) {
	var out []*fletcherv1.EnvVar
	for _, kv := range cmd.StringSlice("env") {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid --env %q (want KEY=VALUE)", kv)
		}
		out = append(out, &fletcherv1.EnvVar{Name: name, Value: value})
	}
	for _, kv := range cmd.StringSlice("secret") {
		name, secret, ok := strings.Cut(kv, "=")
		if !ok || name == "" || secret == "" {
			return nil, fmt.Errorf("invalid --secret %q (want KEY=SECRET_NAME)", kv)
		}
		out = append(out, &fletcherv1.EnvVar{Name: name, SecretName: secret})
	}
	return out, nil
}

func sessionEnvCmd() *cli.Command {
	return &cli.Command{
		Name:  "env",
		Usage: "list, set, and unset a session's environment variables (applies on next start)",
		Commands: []*cli.Command{
			sessionEnvListCmd(),
			sessionEnvSetCmd(),
			sessionEnvUnsetCmd(),
		},
	}
}

func sessionEnvListCmd() *cli.Command {
	return &cli.Command{
		Name:      "list",
		Usage:     "list a session's environment variables",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			vars, err := sessionEnvVars(ctx, cmd, ref)
			if err != nil {
				return err
			}
			if len(vars) == 0 {
				fmt.Println("no environment variables set")
				return nil
			}
			for _, v := range sortedEnv(vars) {
				fmt.Println(formatEnvVar(v))
			}
			return nil
		},
	}
}

func sessionEnvSetCmd() *cli.Command {
	return &cli.Command{
		Name:      "set",
		Usage:     "set (or replace) environment variables, merging with the existing set",
		ArgsUsage: "<ref>",
		Flags:     append([]cli.Flag{socketFlag()}, envVarFlags()...),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			changes, err := parseEnvVarFlags(cmd)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				return errors.New("set --env and/or --secret")
			}
			current, err := sessionEnvVars(ctx, cmd, ref)
			if err != nil {
				return err
			}
			return updateSessionEnv(ctx, cmd, ref, mergeEnv(current, changes))
		},
	}
}

func sessionEnvUnsetCmd() *cli.Command {
	return &cli.Command{
		Name:      "unset",
		Usage:     "remove environment variables by name",
		ArgsUsage: "<ref> <KEY>...",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) < 2 {
				return errors.New("a session ref and at least one variable name are required")
			}
			ref, names := args[0], args[1:]
			current, err := sessionEnvVars(ctx, cmd, ref)
			if err != nil {
				return err
			}
			drop := make(map[string]bool, len(names))
			for _, n := range names {
				drop[n] = true
			}
			kept := current[:0]
			for _, v := range current {
				if !drop[v.GetName()] {
					kept = append(kept, v)
				}
			}
			return updateSessionEnv(ctx, cmd, ref, kept)
		},
	}
}

// sessionEnvVars fetches a session's current environment variables.
func sessionEnvVars(ctx context.Context, cmd *cli.Command, ref string) ([]*fletcherv1.EnvVar, error) {
	resp, err := newSessionsClient(cmd).GetSession(ctx, connect.NewRequest(&fletcherv1.GetSessionRequest{Ref: ref}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession().GetEnvVars(), nil
}

// updateSessionEnv replaces a session's environment with vars and reports the
// outcome (and a restart hint when running).
func updateSessionEnv(ctx context.Context, cmd *cli.Command, ref string, vars []*fletcherv1.EnvVar) error {
	resp, err := newSessionsClient(cmd).UpdateSession(ctx, connect.NewRequest(&fletcherv1.UpdateSessionRequest{
		Ref:           ref,
		EnvVars:       vars,
		UpdateEnvVars: true,
	}))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "updated %s (%d environment variable(s))\n", resp.Msg.GetSession().GetName(), len(vars))
	if resp.Msg.GetRestartRequired() {
		fmt.Println("the session is running; restart it to apply (`fletcher session restart`)")
	}
	return nil
}

// mergeEnv overlays changes onto current by name (a change replaces a same-named
// var), preserving the rest.
func mergeEnv(current, changes []*fletcherv1.EnvVar) []*fletcherv1.EnvVar {
	byName := make(map[string]*fletcherv1.EnvVar, len(current)+len(changes))
	order := make([]string, 0, len(current)+len(changes))
	add := func(v *fletcherv1.EnvVar) {
		if _, ok := byName[v.GetName()]; !ok {
			order = append(order, v.GetName())
		}
		byName[v.GetName()] = v
	}
	for _, v := range current {
		add(v)
	}
	for _, v := range changes {
		add(v)
	}
	out := make([]*fletcherv1.EnvVar, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

func sortedEnv(vars []*fletcherv1.EnvVar) []*fletcherv1.EnvVar {
	out := append([]*fletcherv1.EnvVar(nil), vars...)
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

func formatEnvVar(v *fletcherv1.EnvVar) string {
	if v.GetSecretName() != "" {
		return fmt.Sprintf("%s -> secret:%s", v.GetName(), v.GetSecretName())
	}
	return fmt.Sprintf("%s=%s", v.GetName(), v.GetValue())
}

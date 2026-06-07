package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// proxyStream is the client side of the raw SSH-proxy bidi RPC.
type proxyStream = connect.BidiStreamForClient[fletcherv1.ProxySessionRequest, fletcherv1.ProxySessionResponse]

func sessionSSHCmd() *cli.Command {
	return &cli.Command{
		Name:      "ssh",
		Usage:     "set up SSH (and IDE Remote-SSH) access to a session",
		ArgsUsage: "<ref>",
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			return setupSessionSSH(ctx, cmd, ref)
		},
	}
}

func sessionSSHProxyCmd() *cli.Command {
	return &cli.Command{
		Name:      "ssh-proxy",
		Usage:     "internal: stdio<->session SSH proxy used as an ssh ProxyCommand",
		ArgsUsage: "<ref>",
		Hidden:    true,
		Flags:     []cli.Flag{socketFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return errors.New("session ref (id or name) is required")
			}
			return runSSHProxy(ctx, cmd, ref)
		},
	}
}

// runSSHProxy pipes the local SSH client's stdio to the session's SSH server
// over the daemon's ProxySession stream. It wakes a stopped session first, so
// connecting an IDE to a sleeping session just works.
func runSSHProxy(ctx context.Context, cmd *cli.Command, ref string) error {
	client := newSessionsClient(cmd)
	if _, err := client.StartSession(ctx, connect.NewRequest(&fletcherv1.StartSessionRequest{Ref: ref})); err != nil {
		return err
	}

	stream := client.ProxySession(ctx)
	if err := stream.Send(&fletcherv1.ProxySessionRequest{
		Msg: &fletcherv1.ProxySessionRequest_Open{Open: &fletcherv1.ProxyOpen{Ref: ref}},
	}); err != nil {
		return err
	}

	// stdin -> session
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if serr := stream.Send(&fletcherv1.ProxySessionRequest{
					Msg: &fletcherv1.ProxySessionRequest_Data{Data: append([]byte(nil), buf[:n]...)},
				}); serr != nil {
					return
				}
			}
			if rerr != nil {
				_ = stream.CloseRequest()
				return
			}
		}
	}()

	// session -> stdout
	for {
		resp, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		if _, werr := os.Stdout.Write(resp.GetData()); werr != nil {
			return werr
		}
	}
	_ = stream.CloseResponse()
	return nil
}

// setupSessionSSH ensures a local keypair, installs its public key in the
// session, writes a managed SSH config entry, and prints how to connect.
func setupSessionSSH(ctx context.Context, cmd *cli.Command, ref string) error {
	client := newSessionsClient(cmd)
	sess, err := client.StartSession(ctx, connect.NewRequest(&fletcherv1.StartSessionRequest{Ref: ref}))
	if err != nil {
		return err
	}
	name := sess.Msg.GetSession().GetName()

	pubKey, err := ensureSSHKeypair(ctx)
	if err != nil {
		return err
	}
	if err := installSSHKey(ctx, client, ref, pubKey); err != nil {
		return err
	}

	host := "fletcher-" + name
	if err := writeSSHConfig(host, ref, sshProxyCommand(cmd, ref)); err != nil {
		return err
	}

	// A session recreated under the same ref boots a fresh VM with a new host
	// key. ssh's strict checking accept-news a brand-new host but refuses a
	// *changed* one, so a stale pin from a prior session would block the next
	// connection. This setup is the chokepoint - reinstalling the key here is
	// mandatory after a recreate - so drop the stale pin and let accept-new
	// re-pin cleanly.
	if err := forgetSessionHostKey(ctx, ref); err != nil {
		return err
	}

	fmt.Printf("SSH ready. Connect with:\n\n    ssh %s\n\n", host)
	fmt.Printf("Point an IDE's Remote-SSH at the host %q (it reads your SSH config).\n", host)
	return nil
}

// fletcherSSHDir is where Fletcher keeps its managed SSH keypair, known_hosts,
// and config include.
func fletcherSSHDir() (string, error) {
	dir, err := fletcherConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh"), nil
}

// forgetSessionHostKey evicts any pinned host key for ref from Fletcher's
// managed known_hosts, so a session recreated under the same ref re-pins its
// fresh host key instead of tripping ssh's changed-key guard. Best-effort and
// idempotent: a missing known_hosts means there is nothing to forget. Uses
// ssh-keygen -R (already a hard dependency) so hashed and plain entries are
// handled alike.
func forgetSessionHostKey(ctx context.Context, ref string) error {
	dir, err := fletcherSSHDir()
	if err != nil {
		return err
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	if _, err := os.Stat(knownHosts); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat known_hosts: %w", err)
	}
	out, err := exec.CommandContext(ctx, "ssh-keygen", "-R", ref, "-f", knownHosts).CombinedOutput() //nolint:gosec // fixed args; knownHosts is Fletcher's own managed path
	if err != nil {
		return fmt.Errorf("evict host key for %s: %s", ref, strings.TrimSpace(string(out)))
	}
	// ssh-keygen -R leaves a .old backup beside the file; keep the managed dir tidy.
	_ = os.Remove(knownHosts + ".old")
	return nil
}

// ensureSSHKeypair returns the managed public key, generating the keypair on
// first use.
func ensureSSHKeypair(ctx context.Context) (string, error) {
	dir, err := fletcherSSHDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create ssh dir: %w", err)
	}
	keyPath := filepath.Join(dir, "id_ed25519")
	pubPath := keyPath + ".pub"
	if _, err := os.Stat(keyPath); err != nil {
		// -N "" (no passphrase) so the ProxyCommand is non-interactive; a fixed
		// comment keeps the public key free of shell-significant characters.
		gen := exec.CommandContext(ctx, "ssh-keygen", //nolint:gosec // fixed args; keyPath is Fletcher's own managed path
			"-t", "ed25519", "-N", "", "-C", "fletcher-session", "-f", keyPath)
		gen.Stderr = os.Stderr
		if err := gen.Run(); err != nil {
			return "", fmt.Errorf("generate ssh key: %w", err)
		}
	}
	pub, err := os.ReadFile(pubPath) //nolint:gosec // Fletcher's own managed key path
	if err != nil {
		return "", fmt.Errorf("read public key: %w", err)
	}
	return strings.TrimSpace(string(pub)), nil
}

// installSSHKey appends the public key to the session user's authorized_keys
// (idempotently) via a privileged exec inside the VM.
func installSSHKey(ctx context.Context, client fletcherv1connect.SessionServiceClient, ref, pubKey string) error {
	// pubKey has no single quotes (ed25519 + fixed comment), so single-quoting
	// it for the guest shell is safe.
	script := strings.Join([]string{
		"install -d -m 700 -o fletcher -g fletcher /home/fletcher/.ssh",
		"touch /home/fletcher/.ssh/authorized_keys",
		fmt.Sprintf("grep -qxF '%s' /home/fletcher/.ssh/authorized_keys || echo '%s' >> /home/fletcher/.ssh/authorized_keys", pubKey, pubKey),
		"chown fletcher:fletcher /home/fletcher/.ssh/authorized_keys",
		"chmod 600 /home/fletcher/.ssh/authorized_keys",
	}, " && ")
	resp, err := client.ExecSession(ctx, connect.NewRequest(&fletcherv1.ExecSessionRequest{Ref: ref, Command: script}))
	if err != nil {
		return err
	}
	if code := resp.Msg.GetExitCode(); code != 0 {
		return fmt.Errorf("install ssh key in session: %s", strings.TrimSpace(resp.Msg.GetStderr()))
	}
	return nil
}

// sshProxyCommand builds the ProxyCommand line that ssh runs to reach the
// session, propagating how this invocation targets the daemon.
func sshProxyCommand(cmd *cli.Command, ref string) string {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "fletcher"
	}
	parts := []string{self}
	if remote := cmd.String("remote"); remote != "" {
		parts = append(parts, "--remote", remote)
		if token := cmd.String("token"); token != "" {
			parts = append(parts, "--token", token)
		}
	} else if sock := cmd.String("socket"); sock != "" && sock != defaultSocketPath() {
		parts = append(parts, "--socket", sock)
	}
	parts = append(parts, "session", "ssh-proxy", ref)
	return strings.Join(parts, " ")
}

// writeSSHConfig upserts a Host block for the session into Fletcher's managed
// SSH config, and ensures the user's ~/.ssh/config includes it so `ssh <host>`
// (and IDE Remote-SSH) resolves the host with no further setup.
func writeSSHConfig(host, ref, proxyCommand string) error {
	dir, err := fletcherSSHDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config")
	keyPath := filepath.Join(dir, "id_ed25519")
	knownHosts := filepath.Join(dir, "known_hosts")

	block := fmt.Sprintf(`Host %s
    HostName %s
    User fletcher
    IdentityFile %s
    IdentitiesOnly yes
    StrictHostKeyChecking accept-new
    UserKnownHostsFile %s
    ProxyCommand %s
`, host, ref, keyPath, knownHosts, proxyCommand)

	if err := upsertHostBlock(configPath, host, block); err != nil {
		return err
	}
	return ensureSSHInclude(configPath)
}

// upsertHostBlock rewrites configPath, replacing any existing block for host
// with the new one (appending if absent).
func upsertHostBlock(configPath, host, block string) error {
	existing, err := os.ReadFile(configPath) //nolint:gosec // Fletcher's own managed config path
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read ssh config: %w", err)
	}
	var out strings.Builder
	skip := false
	for _, line := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Host ") {
			skip = trimmed == "Host "+host
		}
		if !skip {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	normalized := strings.TrimRight(out.String(), "\n")
	if normalized != "" {
		normalized += "\n\n"
	}
	normalized += block
	if err := os.WriteFile(configPath, []byte(normalized), 0o600); err != nil {
		return fmt.Errorf("write ssh config: %w", err)
	}
	return nil
}

// ensureSSHInclude adds an Include of Fletcher's managed config to the top of
// the user's ~/.ssh/config (once), so default ssh resolution finds the hosts.
func ensureSSHInclude(configPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create ~/.ssh: %w", err)
	}
	userConfig := filepath.Join(sshDir, "config")
	include := "Include " + configPath

	existing, err := os.ReadFile(userConfig) //nolint:gosec // the user's own ~/.ssh/config
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read ~/.ssh/config: %w", err)
	}
	if strings.Contains(string(existing), include) {
		return nil
	}
	// Include directives must precede Host blocks to apply globally, so prepend.
	merged := include + "\n"
	if len(existing) > 0 {
		merged += "\n" + string(existing)
	}
	if err := os.WriteFile(userConfig, []byte(merged), 0o600); err != nil { //nolint:gosec // the user's own ~/.ssh/config
		return fmt.Errorf("write ~/.ssh/config: %w", err)
	}
	return nil
}

package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/job"
)

// ExportCredential copies an agent login out of a running session into the
// box's credentials root, so future sessions can be created already seeded with
// it - the "log in once" populate path. The session must be running and already
// hold the login (the operator logged in interactively, e.g. with --gateway
// off). The credential directory is tarred out over exec (gzip+base64 so the
// binary survives the text capture) and extracted host-side.
func (m *Manager) ExportCredential(ctx context.Context, ref, name string) error {
	spec, ok := job.Credential(name)
	if !ok {
		return errs.Newf(errs.CategoryInvalidArgument, "unknown credential %q (allowed: %s)",
			name, strings.Join(job.CredentialNames(), ", "))
	}
	if !spec.FromSession {
		return errs.Newf(errs.CategoryInvalidArgument,
			"credential %q is not saved from a session - save it from its own form instead", name)
	}
	root := m.opt().CredentialsRoot
	if root == "" {
		return errs.New(errs.CategoryFailedPrecondition, "the daemon has no credentials root configured")
	}
	// GuestPath = <home>/<HostRelPath>; tar the relpath (and any sibling files
	// like ~/.claude.json) from the home so the archive entries match the host
	// layout under the credentials root. --ignore-failed-read so an absent
	// sibling still saves what is present.
	home := strings.TrimSuffix(spec.GuestPath, "/"+spec.HostRelPath)
	paths := append([]string{spec.HostRelPath}, spec.SiblingFiles...)
	// `test -e <dir> && tar ... | base64`: a missing login short-circuits to a
	// non-zero exit so we report "log in first" instead of writing an empty dir.
	cmd := fmt.Sprintf("test -e %s && tar -cz --ignore-failed-read -C %s %s | base64 -w0",
		spec.GuestPath, home, strings.Join(paths, " "))
	res, err := m.Exec(ctx, ref, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return errs.Newf(errs.CategoryFailedPrecondition,
			"session %q has no %s login to save - run the agent and log in first", ref, name)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil {
		return fmt.Errorf("decode exported %s credential: %w", name, err)
	}
	if err := extractTarGz(data, root); err != nil {
		return fmt.Errorf("save %s credential to %s: %w", name, root, err)
	}
	return nil
}

// SavedCredentials lists the box's saved logins (those with files under the
// credentials root).
func (m *Manager) SavedCredentials() []string {
	return job.SavedCredentials(m.opt().CredentialsRoot)
}

// SupportedCredentials lists every agent whose login can be saved out of a
// running session (the catalog), so clients drive their "save login from a
// session" picker from this rather than a hardcoded list that drifts from what
// the image ships. The git login is excluded - it is saved via SaveGitCredential.
func (m *Manager) SupportedCredentials() []string {
	return job.SessionLoginNames()
}

// SaveGitCredential saves a git host login (host + username + token, plus an
// optional committer identity) under the box's credentials root, so new
// sessions seeded with the "git" credential clone over HTTPS. No running
// session is needed - the credential is built from the given fields.
func (m *Manager) SaveGitCredential(host, username, token, gitName, gitEmail string) error {
	return job.WriteGitCredential(m.opt().CredentialsRoot, host, username, token, gitName, gitEmail)
}

// DeleteSavedCredential removes a saved login from the credentials root.
func (m *Manager) DeleteSavedCredential(name string) error {
	return job.DeleteSavedCredential(m.opt().CredentialsRoot, name)
}

// extractTarGz extracts a gzip-compressed tar into dst, refusing any entry that
// would escape dst (path traversal) and skipping anything that is not a plain
// directory or regular file. Entry sizes bound each copy, so a malformed
// archive cannot exhaust disk.
func extractTarGz(data []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if clean == "." {
			continue
		}
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("refusing unsafe path %q in credential archive", hdr.Name)
		}
		target := filepath.Join(dst, clean)
		if rel, err := filepath.Rel(dst, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("refusing path %q escaping the credentials root", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := writeTarFile(target, tr, hdr.Size); err != nil {
				return err
			}
		default:
			// Skip symlinks, devices, etc. - credential dirs are plain files.
		}
	}
}

// writeTarFile writes one extracted credential file at 0600 - tokens and config
// are owner-only by nature, so the saved copy is locked down regardless of the
// archive's bits. target was validated to stay under the credentials root.
func writeTarFile(target string, tr io.Reader, size int64) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // target is validated to stay under the daemon-owned credentials root
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, tr, size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

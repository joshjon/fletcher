package job

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
)

// Credential names accepted by the trusted-credential mode.
//
// Agent login seeding (claude/codex/gemini/pi) was removed: a saved agent login
// is a frozen OAuth snapshot whose access token expires within hours and whose
// refresh token is invalidated soon after the save, so a seeded session boots
// unauthenticated every time (docs/ROADMAP.md M16). Git remains - it is a static
// token, not a living OAuth credential, so seeding it is durable.
const (
	// CredentialGit is the vendor-neutral git HTTPS login (host + username +
	// token), saved from structured fields - see WriteGitCredential.
	CredentialGit = "git"
)

// AllowedCredential describes one mountable credential directory.
type AllowedCredential struct {
	// Name is the user-facing identifier on the wire and CLI.
	Name string
	// HostRelPath is the credential directory's path relative to the daemon's
	// configured credentials root (typically the operator's $HOME).
	HostRelPath string
	// GuestPath is the fixed mount point inside the fletcher-base image,
	// matching the paths the bundled agent CLIs read by default.
	GuestPath string
	// SiblingFiles are extra home-relative files (not under HostRelPath) that a
	// login also needs, seeded alongside the credential dir. Unused today (git
	// is a single dir), kept as generic infra for any future credential that
	// splits its state across sibling files.
	SiblingFiles []string
}

// homeRel is the login user's home, the root SiblingFiles and GuestPath share.
const homeRel = "/home/fletcher"

// AllowedCredentials lists every credential the trusted-credential mode
// supports. The supervisor resolves names → AllowedCredential entries at
// job-start time.
var AllowedCredentials = map[string]AllowedCredential{
	// git is one self-contained XDG dir: a `credentials` file (one
	// https://user:token@host line per host) and a `config` file (store helper +
	// committer identity), saved via the form (WriteGitCredential).
	CredentialGit: {Name: CredentialGit, HostRelPath: ".config/git", GuestPath: homeRel + "/.config/git"},
}

// SavedCredentials lists the credential names that have files saved under root
// (the box's saved logins), so a client can show which logins exist.
func SavedCredentials(root string) []string {
	if root == "" {
		return nil
	}
	var out []string
	for _, name := range CredentialNames() {
		base := filepath.Join(root, AllowedCredentials[name].HostRelPath)
		if entries, err := os.ReadDir(base); err == nil && len(entries) > 0 {
			out = append(out, name)
		}
	}
	return out
}

// DeleteSavedCredential removes a saved login's files from under root.
func DeleteSavedCredential(root, name string) error {
	spec, ok := AllowedCredentials[name]
	if !ok {
		return errs.Newf(errs.CategoryInvalidArgument, "unknown credential %q (allowed: %s)", name, allowedCredentialNames())
	}
	if root == "" {
		return errs.New(errs.CategoryFailedPrecondition, "the daemon has no credentials root configured")
	}
	if err := os.RemoveAll(filepath.Join(root, spec.HostRelPath)); err != nil {
		return fmt.Errorf("delete saved credential %q: %w", name, err)
	}
	return nil
}

// Credential returns the catalog entry for a credential name.
func Credential(name string) (AllowedCredential, bool) {
	c, ok := AllowedCredentials[name]
	return c, ok
}

// WriteGitCredential saves a git host login under the box's credentials root as
// the vendor-neutral "git" credential, so a session seeded with it clones over
// HTTPS with no prompt. It writes git's XDG config dir (~/.config/git): a
// `credentials` file holding one `https://user:token@host` line per host
// (git-credential-store's default search path) and a `config` file enabling
// that store helper plus any committer identity. Call once per host - an
// existing line for the same host is replaced and other hosts are kept, so
// github.com and gitlab.com coexist. A blank name/email leaves any previously
// saved identity untouched.
func WriteGitCredential(root, host, username, token, gitName, gitEmail string) error {
	if root == "" {
		return errs.New(errs.CategoryFailedPrecondition, "the daemon has no credentials root configured")
	}
	host = strings.TrimSpace(host)
	username = strings.TrimSpace(username)
	token = strings.TrimSpace(token)
	if host == "" || username == "" || token == "" {
		return errs.New(errs.CategoryInvalidArgument, "git credential needs a host, username, and token")
	}
	// host matches the store by scheme+host, so a scheme or path here would never
	// match a clone URL - reject it rather than silently never authenticating.
	if strings.Contains(host, "://") || strings.ContainsAny(host, "/@ ") {
		return errs.Newf(errs.CategoryInvalidArgument,
			"git host %q must be a bare hostname like github.com (no scheme, no path)", host)
	}
	dir := filepath.Join(root, ".config", "git")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create git credential dir: %w", err)
	}
	if err := upsertGitCredentialLine(filepath.Join(dir, "credentials"), host, username, token); err != nil {
		return fmt.Errorf("write git credentials: %w", err)
	}
	if err := writeGitConfig(filepath.Join(dir, "config"), gitName, gitEmail); err != nil {
		return fmt.Errorf("write git config: %w", err)
	}
	return nil
}

// upsertGitCredentialLine writes the store line for host into path, replacing
// any existing line for the same host and keeping the rest. url.UserPassword
// percent-encodes the credentials, which git-credential-store decodes on read.
func upsertGitCredentialLine(path, host, username, token string) error {
	line := (&url.URL{Scheme: "https", User: url.UserPassword(username, token), Host: host}).String()
	var kept []string
	switch data, err := os.ReadFile(path); { //nolint:gosec // path is under the daemon-owned credentials root
	case err == nil:
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l == "" {
				continue
			}
			if u, perr := url.Parse(l); perr == nil && u.Host == host {
				continue // replaced by the new line below
			}
			kept = append(kept, l)
		}
	case !os.IsNotExist(err):
		return err
	}
	kept = append(kept, line)
	//nolint:gosec // path is the daemon-owned credentials file, not user input
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

// gitIdentity is the committer name/email writeGitConfig manages under [user].
type gitIdentity struct{ name, email string }

// writeGitConfig writes ~/.config/git/config enabling the credential store
// helper and, when set, the committer identity. A blank name/email is filled
// from the existing file so saving a second host does not wipe the identity.
func writeGitConfig(path, name, email string) error {
	cur := readGitIdentity(path)
	if name == "" {
		name = cur.name
	}
	if email == "" {
		email = cur.email
	}
	var b strings.Builder
	b.WriteString("[credential]\n\thelper = store\n")
	if name != "" || email != "" {
		b.WriteString("[user]\n")
		if name != "" {
			fmt.Fprintf(&b, "\tname = %s\n", name)
		}
		if email != "" {
			fmt.Fprintf(&b, "\temail = %s\n", email)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// readGitIdentity recovers the user.name / user.email that writeGitConfig
// previously wrote. It only understands the shape writeGitConfig emits
// (tab-indented `name = ` / `email = ` under [user]); this package fully owns
// the file, so anything else is safe to ignore.
func readGitIdentity(path string) gitIdentity {
	data, err := os.ReadFile(path) //nolint:gosec // path is under the daemon-owned credentials root
	if err != nil {
		return gitIdentity{}
	}
	var id gitIdentity
	inUser := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "[user]":
			inUser = true
		case strings.HasPrefix(line, "["):
			inUser = false
		case inUser && strings.HasPrefix(line, "name ="):
			id.name = strings.TrimSpace(strings.TrimPrefix(line, "name ="))
		case inUser && strings.HasPrefix(line, "email ="):
			id.email = strings.TrimSpace(strings.TrimPrefix(line, "email ="))
		}
	}
	return id
}

// CredentialNames returns every supported credential name, sorted.
func CredentialNames() []string {
	names := make([]string, 0, len(AllowedCredentials))
	for name := range AllowedCredentials {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveCredentialFiles reads the named credential directories under root and
// returns their contents as seedable files: each regular file under
// <root>/<HostRelPath> maps to <GuestPath>/<rel>, carrying its permission bits.
// Sessions seed these into a fork at create so a new session boots already
// logged in (the Firecracker runtime cannot bind-mount a host dir, so it copies
// the files in). Unknown names and a missing host directory error - the
// operator asked to seed a login that is not present.
func ResolveCredentialFiles(root string, names []string) ([]runtime.CredentialFile, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if root == "" {
		return nil, errs.Newf(errs.CategoryFailedPrecondition,
			"credentials %v requested but the daemon has no credentials root configured", names)
	}
	var out []runtime.CredentialFile
	for _, name := range names {
		spec, ok := AllowedCredentials[name]
		if !ok {
			return nil, errs.Newf(errs.CategoryInvalidArgument, "unknown credential %q (allowed: %s)", name, allowedCredentialNames())
		}
		files, err := resolveOneCredential(root, spec)
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}

// resolveOneCredential reads one credential's directory (and any sibling files)
// under root into seedable files.
func resolveOneCredential(root string, spec AllowedCredential) ([]runtime.CredentialFile, error) {
	base := filepath.Join(root, spec.HostRelPath)
	info, err := os.Stat(base)
	if err != nil {
		return nil, errs.Newf(errs.CategoryFailedPrecondition,
			"credential %q not found at %s (save it with `fletcher credential save %s`)", spec.Name, base, spec.Name)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("credential %q: host path %s is not a directory", spec.Name, base)
	}
	out, err := walkCredentialDir(base, spec.GuestPath)
	if err != nil {
		return nil, fmt.Errorf("credential %q: read %s: %w", spec.Name, base, err)
	}
	// Sibling files (e.g. ~/.claude.json) live next to the dir, not under it.
	// Skip any that are absent so a partial login still seeds what it has.
	for _, rel := range spec.SiblingFiles {
		src := filepath.Join(root, rel)
		data, err := os.ReadFile(src) //nolint:gosec // src is under the daemon-owned credentials root
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("credential %q: read %s: %w", spec.Name, src, err)
		}
		out = append(out, runtime.CredentialFile{Path: path.Join(homeRel, filepath.ToSlash(rel)), Mode: 0o600, Data: data})
	}
	return out, nil
}

// walkCredentialDir reads every regular file under base into a CredentialFile
// rooted at guestPath, preserving each file's permission bits.
func walkCredentialDir(base, guestPath string) ([]runtime.CredentialFile, error) {
	var out []runtime.CredentialFile
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		// Skip directories and anything not a regular file (sockets, symlinks).
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // p is under the daemon-owned credentials root
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, runtime.CredentialFile{
			Path: path.Join(guestPath, filepath.ToSlash(rel)),
			Mode: uint32(fi.Mode().Perm()),
			Data: data,
		})
		return nil
	})
	return out, err
}

// ValidateCredentialNames checks each name against AllowedCredentials, returning
// a sorted, de-duplicated list (exported wrapper over the job-internal
// normaliser so the session path can validate a session's requested
// credentials the same way).
func ValidateCredentialNames(in []string) ([]string, error) {
	return normaliseCredentials(in)
}

// normaliseCredentials validates each name against AllowedCredentials,
// removes duplicates, and returns the result sorted for stable storage.
func normaliseCredentials(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	for _, name := range in {
		if _, ok := AllowedCredentials[name]; !ok {
			return nil, errs.Newf(errs.CategoryInvalidArgument, "unknown credential %q (allowed: %s)", name, allowedCredentialNames())
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// encodeCredentials serialises the (already-validated, sorted) list to the
// JSON form stored in SQLite. An empty list maps to the empty string so the
// SQL DEFAULT for the column is the canonical "none" value.
func encodeCredentials(creds []string) (string, error) {
	if len(creds) == 0 {
		return "", nil
	}
	b, err := json.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("encode credentials: %w", err)
	}
	return string(b), nil
}

// decodeCredentials parses the JSON form stored in SQLite into a slice.
// The empty string round-trips to a nil slice.
func decodeCredentials(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	return out, nil
}

func allowedCredentialNames() string {
	names := make([]string, 0, len(AllowedCredentials))
	for name := range AllowedCredentials {
		names = append(names, name)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

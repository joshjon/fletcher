package job

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/runtime"
)

// Credential names accepted by the trusted-credential mode.
const (
	CredentialClaude = "claude"
	CredentialCodex  = "codex"
	CredentialPi     = "pi"
	CredentialGemini = "gemini"
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
	// SiblingFiles are extra home-relative files (not under HostRelPath) that an
	// agent's login also needs. Claude Code splits its state between ~/.claude/
	// (tokens) and ~/.claude.json (account, onboarding), so seeding the dir alone
	// leaves a session re-prompting for login. Session seed/export carry these;
	// the job bind-mount path (HostRelPath only) does not.
	SiblingFiles []string
}

// homeRel is the login user's home, the root SiblingFiles and GuestPath share.
const homeRel = "/home/fletcher"

// AllowedCredentials lists every credential the trusted-credential mode
// supports. The supervisor resolves names → AllowedCredential entries at
// job-start time.
var AllowedCredentials = map[string]AllowedCredential{
	CredentialClaude: {Name: CredentialClaude, HostRelPath: ".claude", GuestPath: homeRel + "/.claude", SiblingFiles: []string{".claude.json"}},
	CredentialCodex:  {Name: CredentialCodex, HostRelPath: ".codex", GuestPath: homeRel + "/.codex"},
	CredentialPi:     {Name: CredentialPi, HostRelPath: ".pi", GuestPath: homeRel + "/.pi"},
	CredentialGemini: {Name: CredentialGemini, HostRelPath: ".gemini", GuestPath: homeRel + "/.gemini"},
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

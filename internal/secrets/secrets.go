// Package secrets owns the daemon's age-encrypted secret store. Plaintext
// values are decrypted only inside the daemon process (never in forks —
// see DESIGN.md §5/§6). The age identity itself lives on disk at a
// configurable path; the daemon auto-generates it on first boot.
package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/joshjon/fletcher/internal/errs"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// ErrNotFound is returned when a requested secret name does not exist.
var ErrNotFound = errs.New(errs.CategoryNotFound, "secret not found")

// Metadata is the non-sensitive subset of a secret's record.
type Metadata struct {
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store mediates encrypted access to the daemon's secrets. Set encrypts;
// Get decrypts (lazily caching plaintext in memory). The age identity is
// loaded once from disk at construction.
type Store struct {
	q         sqliteq.Querier
	identity  *age.X25519Identity
	recipient age.Recipient

	mu    sync.RWMutex
	cache map[string]string
}

// Open returns a Store backed by q, loading the age identity from
// identityPath. If the file does not exist a fresh identity is generated,
// written to identityPath with mode 0o600, and used.
func Open(q sqliteq.Querier, identityPath string) (*Store, error) {
	identity, err := loadOrGenerateIdentity(identityPath)
	if err != nil {
		return nil, err
	}
	return &Store{
		q:         q,
		identity:  identity,
		recipient: identity.Recipient(),
		cache:     make(map[string]string),
	}, nil
}

// Set encrypts value under the store's age recipient and persists it.
func (s *Store) Set(ctx context.Context, name, value string) error {
	if name == "" {
		return errs.New(errs.CategoryInvalidArgument, "secret name is required")
	}
	ciphertext, err := s.encrypt(value)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	now := time.Now().Unix()
	if err := s.q.UpsertSecret(ctx, sqliteq.UpsertSecretParams{
		Name:       name,
		Ciphertext: ciphertext,
		CreatedAt:  now,
		UpdatedAt:  now,
	}); err != nil {
		return fmt.Errorf("upsert secret: %w", err)
	}
	s.mu.Lock()
	s.cache[name] = value
	s.mu.Unlock()
	return nil
}

// Get returns the decrypted plaintext for name. Values are cached in
// memory after first decryption to amortise the cost over hot paths
// (the model-gateway reads the LLM API key per request).
func (s *Store) Get(ctx context.Context, name string) (string, error) {
	s.mu.RLock()
	if v, ok := s.cache[name]; ok {
		s.mu.RUnlock()
		return v, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.cache[name]; ok {
		return v, nil
	}

	ciphertext, err := s.q.GetSecret(ctx, name)
	if err != nil {
		if isSQLNotFound(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("get secret: %w", err)
	}
	plain, err := s.decrypt(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	s.cache[name] = plain
	return plain, nil
}

// Delete removes a secret. Missing secrets are not an error.
func (s *Store) Delete(ctx context.Context, name string) error {
	if _, err := s.q.DeleteSecret(ctx, name); err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	s.mu.Lock()
	delete(s.cache, name)
	s.mu.Unlock()
	return nil
}

// List returns metadata for every stored secret, sorted by name. Plaintext
// is never returned in bulk.
func (s *Store) List(ctx context.Context) ([]Metadata, error) {
	rows, err := s.q.ListSecretMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	out := make([]Metadata, len(rows))
	for i, r := range rows {
		out[i] = Metadata{
			Name:      r.Name,
			CreatedAt: time.Unix(r.CreatedAt, 0).UTC(),
			UpdatedAt: time.Unix(r.UpdatedAt, 0).UTC(),
		}
	}
	return out, nil
}

// Recipient returns the public half of the age identity. Useful for tools
// (out of scope today) that want to encrypt-for-us without holding the
// private key.
func (s *Store) Recipient() age.Recipient { return s.recipient }

func (s *Store) encrypt(plain string) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, s.recipient)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Store) decrypt(ciphertext []byte) (string, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), s.identity)
	if err != nil {
		return "", err
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func loadOrGenerateIdentity(path string) (*age.X25519Identity, error) {
	data, err := os.ReadFile(path) //nolint:gosec // identity file path is operator-supplied
	switch {
	case err == nil:
		ids, err := age.ParseIdentities(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("parse identity at %s: %w", path, err)
		}
		for _, id := range ids {
			if x, ok := id.(*age.X25519Identity); ok {
				return x, nil
			}
		}
		return nil, fmt.Errorf("identity at %s has no X25519 keys", path)

	case errors.Is(err, os.ErrNotExist):
		return generateIdentityFile(path)

	default:
		return nil, fmt.Errorf("read identity %s: %w", path, err)
	}
}

func generateIdentityFile(path string) (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create identity directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write identity: %w", err)
	}
	return id, nil
}

// isSQLNotFound matches the sql.ErrNoRows path without importing
// database/sql here (sqlc returns the typed sentinel back).
func isSQLNotFound(err error) bool {
	return err != nil && err.Error() == "sql: no rows in result set"
}

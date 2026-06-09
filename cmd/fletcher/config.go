package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// clientConfig is the CLI's stored client-side configuration: which remote
// daemon to drive and the per-peer token to authenticate with. It is written by
// `fletcher login` and read as the lowest-precedence source for the remote
// target - explicit --remote/--token flags and the FLETCHER_REMOTE/FLETCHER_TOKEN
// env vars override it.
type clientConfig struct {
	Remote string `toml:"remote"`
	Token  string `toml:"token"`
}

// fletcherConfigDir is ~/.config/fletcher, the CLI's per-user config home (also
// home to the managed SSH artifacts under ssh/).
func fletcherConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "fletcher"), nil
}

func clientConfigPath() (string, error) {
	dir, err := fletcherConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// loadClientConfig reads the stored client config, returning a zero config when
// it is absent or unreadable - a missing login is the normal local-socket case,
// not an error.
func loadClientConfig() clientConfig {
	path, err := clientConfigPath()
	if err != nil {
		return clientConfig{}
	}
	var cfg clientConfig
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return clientConfig{}
	}
	return cfg
}

// saveClientConfig writes cfg at mode 0600 - it holds a bearer token.
func saveClientConfig(cfg clientConfig) error {
	dir, err := fletcherConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "config.toml"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // Fletcher's own managed config path
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
}

// clearClientConfig removes the stored credentials (used by `fletcher logout`),
// reporting whether anything was removed.
func clearClientConfig() (bool, error) {
	path, err := clientConfigPath()
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// loginBlob is the compact, copy-paste credential `peer pair` prints and
// `fletcher login` consumes: the remote API endpoint plus the per-peer token,
// so configuring a client is copying one string.
type loginBlob struct {
	Remote string `json:"r"`
	Token  string `json:"t"`
}

func encodeLoginBlob(remote, token string) string {
	b, _ := json.Marshal(loginBlob{Remote: remote, Token: token})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeLoginBlob(s string) (loginBlob, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return loginBlob{}, errors.New("not a valid login token")
	}
	var blob loginBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		return loginBlob{}, errors.New("not a valid login token")
	}
	if blob.Remote == "" || blob.Token == "" {
		return loginBlob{}, errors.New("login token is missing its remote or token")
	}
	return blob, nil
}

// pairBlobVersion is the schema version of the pair blob produced by
// `fletcher peer pair --mobile`. Native clients (iOS) refuse to decode
// other versions; bump on any incompatible field change.
const pairBlobVersion = 1

// pairBlob is the unified payload emitted by `fletcher peer pair
// --mobile` for native clients that do their own WireGuard keygen.
// Field tags are short to keep the QR small.
type pairBlob struct {
	Version             int      `json:"v"`
	PairingCode         string   `json:"code"`
	ExpiresAt           int64    `json:"exp"`
	ServerPublicKey     string   `json:"spk"`
	Endpoint            string   `json:"ep"`
	Address             string   `json:"addr"`
	AllowedIPs          []string `json:"aips"`
	APIEndpoint         string   `json:"api"`
	PersistentKeepalive int32    `json:"ka"`
	Name                string   `json:"name"`
	// PairingEndpoint is the public host:port the app POSTs CompletePair
	// to over TLS, before the tunnel exists. PairingFingerprint is the
	// lowercase hex SHA-256 of that endpoint's leaf certificate, which the
	// app pins (the QR is the out-of-band trust anchor).
	PairingEndpoint    string `json:"pep"`
	PairingFingerprint string `json:"fp"`
}

func encodePairBlob(b pairBlob) string {
	b.Version = pairBlobVersion
	raw, _ := json.Marshal(b)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodePairBlob(s string) (pairBlob, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return pairBlob{}, errors.New("not a valid pair blob")
	}
	var b pairBlob
	if err := json.Unmarshal(raw, &b); err != nil {
		return pairBlob{}, errors.New("not a valid pair blob")
	}
	if b.Version != pairBlobVersion {
		return pairBlob{}, fmt.Errorf("unsupported pair blob version %d", b.Version)
	}
	if b.PairingCode == "" || b.ServerPublicKey == "" || b.Endpoint == "" || b.Address == "" {
		return pairBlob{}, errors.New("pair blob is missing required fields")
	}
	if b.PairingEndpoint == "" || b.PairingFingerprint == "" {
		return pairBlob{}, errors.New("pair blob is missing the pairing endpoint or its TLS fingerprint (daemon has no public pairing listener)")
	}
	return b, nil
}

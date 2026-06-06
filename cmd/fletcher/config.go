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

// Package push delivers notifications to Apple's APNs directly from the box
// (no Fletcher-operated relay), using the operator's own APNs auth key. The
// payloads are content-light: a generic alert plus an id the app uses to fetch
// the real detail over the tunnel, so nothing sensitive transits Apple.
package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	apnsProdHost    = "https://api.push.apple.com"
	apnsSandboxHost = "https://api.sandbox.push.apple.com"
)

// Config configures the APNs sender; every field is required to send.
type Config struct {
	// KeyPath is the path to the APNs auth key (.p8) on the box.
	KeyPath string
	// KeyID is that key's ID (Apple Developer).
	KeyID string
	// TeamID is the Apple Developer team ID.
	TeamID string
	// Topic is the app's bundle ID (sent as apns-topic).
	Topic string
	// Sandbox uses Apple's sandbox APNs host (for development builds).
	Sandbox bool
}

// Sender pushes notifications to APNs. Safe for concurrent use.
type Sender struct {
	key    *ecdsa.PrivateKey
	keyID  string
	teamID string
	topic  string
	host   string
	client *http.Client

	mu     sync.Mutex
	jwt    string
	jwtExp time.Time
}

// NewSender parses the .p8 auth key and builds a Sender. It errors when the
// config is incomplete or the key cannot be parsed.
func NewSender(cfg Config) (*Sender, error) {
	if cfg.KeyPath == "" || cfg.KeyID == "" || cfg.TeamID == "" || cfg.Topic == "" {
		return nil, errors.New("apns: key path, key id, team id, and topic are all required")
	}
	pemBytes, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("apns: read key: %w", err)
	}
	key, err := parseP8(pemBytes)
	if err != nil {
		return nil, err
	}
	host := apnsProdHost
	if cfg.Sandbox {
		host = apnsSandboxHost
	}
	return &Sender{
		key:    key,
		keyID:  cfg.KeyID,
		teamID: cfg.TeamID,
		topic:  cfg.Topic,
		host:   host,
		client: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func parseP8(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("apns: key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: parse key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: key is not an ECDSA key")
	}
	return ec, nil
}

// Notification is the content-light push payload.
type Notification struct {
	Title string
	Body  string
	// Data carries custom string key/values (e.g. an approval id) to the app.
	Data map[string]string
}

// SendResult reports the outcome for one token.
type SendResult struct {
	// Gone is true when APNs reports the token is permanently invalid
	// (BadDeviceToken / Unregistered), so the caller should drop it.
	Gone bool
}

// Send delivers n to a device token. A non-nil error is a transient failure;
// SendResult.Gone marks a token APNs says is dead.
func (s *Sender) Send(ctx context.Context, token string, n Notification) (SendResult, error) {
	body, err := buildPayload(n)
	if err != nil {
		return SendResult{}, err
	}
	jwt, err := s.providerToken()
	if err != nil {
		return SendResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.host+"/3/device/"+token, bytes.NewReader(body))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", s.topic)
	req.Header.Set("apns-push-type", "alert")
	resp, err := s.client.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("apns: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return SendResult{}, nil
	case http.StatusGone, http.StatusBadRequest:
		reason := apnsReason(resp.Body)
		if reason == "Unregistered" || reason == "BadDeviceToken" {
			return SendResult{Gone: true}, nil
		}
		return SendResult{}, fmt.Errorf("apns: rejected (%s)", reason)
	default:
		return SendResult{}, fmt.Errorf("apns: status %d (%s)", resp.StatusCode, apnsReason(resp.Body))
	}
}

func apnsReason(r io.Reader) string {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r).Decode(&body)
	if body.Reason == "" {
		return "unknown"
	}
	return body.Reason
}

func buildPayload(n Notification) ([]byte, error) {
	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{"title": n.Title, "body": n.Body},
			"sound": "default",
		},
	}
	for k, v := range n.Data {
		payload[k] = v
	}
	return json.Marshal(payload)
}

// providerToken returns a cached APNs provider JWT, refreshing it before APNs's
// 1-hour cap.
func (s *Sender) providerToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jwt != "" && time.Now().Before(s.jwtExp) {
		return s.jwt, nil
	}
	now := time.Now()
	jwt, err := signJWT(s.key, s.keyID, s.teamID, now)
	if err != nil {
		return "", err
	}
	s.jwt, s.jwtExp = jwt, now.Add(50*time.Minute)
	return jwt, nil
}

// signJWT builds the ES256 provider authentication token APNs expects.
func signJWT(key *ecdsa.PrivateKey, keyID, teamID string, now time.Time) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": keyID})
	claims, _ := json.Marshal(map[string]any{"iss": teamID, "iat": now.Unix()})
	signingInput := b64(header) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, ss, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("apns: sign jwt: %w", err)
	}
	// ES256: the signature is R||S, each left-padded to 32 bytes (P-256).
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

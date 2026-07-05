package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func p256PEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestParseP8(t *testing.T) {
	t.Run("valid pkcs8 p-256 key parses", func(t *testing.T) {
		key, err := parseP8(p256PEM(t))
		require.NoError(t, err)
		require.NotNil(t, key)
		require.Equal(t, elliptic.P256(), key.Curve)
	})

	t.Run("non-pem input is rejected", func(t *testing.T) {
		_, err := parseP8([]byte("definitely not a pem file"))
		require.Error(t, err)
		require.ErrorContains(t, err, "key is not PEM")
	})

	t.Run("pem wrapping garbage der is rejected", func(t *testing.T) {
		bogus := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{0x30, 0x01, 0xff}})
		_, err := parseP8(bogus)
		require.Error(t, err)
		require.ErrorContains(t, err, "parse key")
	})

	t.Run("rsa key is the wrong type", func(t *testing.T) {
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
		require.NoError(t, err)
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

		_, err = parseP8(pemBytes)
		require.Error(t, err)
		require.ErrorContains(t, err, "not an ECDSA key")
	})
}

func TestAPNSReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "reason present", body: `{"reason":"Unregistered"}`, want: "Unregistered"},
		{name: "empty body", body: "", want: "unknown"},
		{name: "invalid json", body: "<html>oops</html>", want: "unknown"},
		{name: "json without a reason", body: `{"timestamp":123}`, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, apnsReason(strings.NewReader(tt.body)))
		})
	}
}

// newTestSender builds a Sender pointed at an httptest server instead of
// Apple, exercising the real request build + response handling paths.
func newTestSender(t *testing.T, handler http.HandlerFunc) *Sender {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Sender{
		key:    key,
		keyID:  "KEYID123",
		teamID: "TEAMID456",
		topic:  "dev.example.app",
		host:   srv.URL,
		client: srv.Client(),
	}
}

func TestSendSuccess(t *testing.T) {
	var gotPath, gotTopic, gotPushType, gotAuth string
	var gotBody []byte
	s := newTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTopic = r.Header.Get("apns-topic")
		gotPushType = r.Header.Get("apns-push-type")
		gotAuth = r.Header.Get("authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	res, err := s.Send(context.Background(), "device-token-1", Notification{
		Title: "Fletcher",
		Body:  "An action needs your approval.",
		Data:  map[string]string{"approval_id": "approval_123"},
	})
	require.NoError(t, err)
	require.False(t, res.Gone)

	require.Equal(t, "/3/device/device-token-1", gotPath)
	require.Equal(t, "dev.example.app", gotTopic)
	require.Equal(t, "alert", gotPushType)
	require.True(t, strings.HasPrefix(gotAuth, "bearer "), "authorization must carry a bearer jwt")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &payload))
	require.Equal(t, "approval_123", payload["approval_id"])
	require.Contains(t, payload, "aps")
}

func TestSendGoneTokens(t *testing.T) {
	tests := []struct {
		name   string
		status int
		reason string
	}{
		{name: "410 unregistered", status: http.StatusGone, reason: "Unregistered"},
		{name: "400 bad device token", status: http.StatusBadRequest, reason: "BadDeviceToken"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"reason":"` + tt.reason + `"}`))
			})
			res, err := s.Send(context.Background(), "dead-token", Notification{Title: "t"})
			require.NoError(t, err, "a permanently dead token is a result, not an error")
			require.True(t, res.Gone)
		})
	}
}

func TestSendFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{
			name:    "400 with another reason is rejected",
			status:  http.StatusBadRequest,
			body:    `{"reason":"BadMessageId"}`,
			wantErr: "rejected (BadMessageId)",
		},
		{
			name:    "429 maps status and reason",
			status:  http.StatusTooManyRequests,
			body:    `{"reason":"TooManyRequests"}`,
			wantErr: "status 429 (TooManyRequests)",
		},
		{
			name:    "500 without a body reports unknown",
			status:  http.StatusInternalServerError,
			body:    "",
			wantErr: "status 500 (unknown)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestSender(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			res, err := s.Send(context.Background(), "token", Notification{Title: "t"})
			require.Error(t, err)
			require.ErrorContains(t, err, tt.wantErr)
			require.False(t, res.Gone)
		})
	}
}

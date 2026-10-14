package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/settings"
)

func publicWebMode(cfg Config) string {
	if cfg.PublicWebMode == "" {
		return "edge"
	}
	return cfg.PublicWebMode
}

func publicWebListen(cfg Config) string {
	if cfg.PublicWebListen == "" {
		return settings.DefaultPublicWebListen
	}
	return cfg.PublicWebListen
}

func validatePublicWebConfig(cfg *Config) error {
	cfg.PublicWebMode = publicWebMode(*cfg)
	cfg.PublicWebListen = publicWebListen(*cfg)
	var problems []error
	if cfg.PublicWebMode != "edge" && cfg.PublicWebMode != "behind_proxy" {
		problems = append(problems, fmt.Errorf("public_web_mode must be edge or behind_proxy"))
	}
	if err := settings.ValidatePublicWebListen(cfg.PublicWebListen); err != nil {
		problems = append(problems, err)
	}
	if _, err := settings.ParsePublicWebTrustedProxies(cfg.PublicWebTrustedProxies); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

func buildBehindProxyWeb(ctx context.Context, cfg Config, backend session.PublicBackend, logger *slog.Logger) *publicWebServers {
	trusted, err := settings.ParsePublicWebTrustedProxies(cfg.PublicWebTrustedProxies)
	if err != nil {
		logger.Error("public web disabled: invalid trusted proxies", slog.String("err", err.Error()))
		return nil
	}
	ln, _, err := listenTCP(ctx, cfg.PublicWebListen, "public-proxy")
	if err != nil {
		logger.Error("public web disabled: cannot bind proxy listener", slog.String("err", err.Error()))
		return nil
	}
	pub := session.NewBehindProxyServer(session.PublicConfig{Backend: backend, Logger: logger}, trusted)
	logger.Info("public web serving behind proxy; operator owns TLS and access control",
		slog.String("http", ln.Addr().String()))
	return &publicWebServers{
		httpSrv: &http.Server{Handler: pub.AppHandler(), ReadHeaderTimeout: 10 * time.Second},
		httpLn:  ln,
		pub:     pub,
	}
}

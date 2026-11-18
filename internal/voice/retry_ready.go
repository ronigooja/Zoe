package voice

import (
	"context"
	"os"

	"github.com/ronigooja/Zoe/internal/backoff"
	"github.com/ronigooja/Zoe/internal/logging"
)

// RetryModelConfig identifies the loaded model and its credential source.
type RetryModelConfig struct {
	Endpoint, Model, Effort, Thinking, CredentialFile string
}

// ReadyHandle exposes post-migration service initialization to the server.
type ReadyHandle struct{ service *Service }

// AfterMigrate initializes retry configuration after database migrations.
func (h *ReadyHandle) AfterMigrate(ctx context.Context) error {
	if h == nil || h.service == nil {
		return nil
	}
	return h.service.AfterMigrate(ctx)
}

// AfterMigrate observes loaded model configuration after database migrations.
func (s *Service) AfterMigrate(ctx context.Context) error {
	s.retryReadyMu.Lock()
	defer s.retryReadyMu.Unlock()
	if s.retryReady {
		return nil
	}
	for _, consumer := range []string{"register", "steward", "ui_controller", "compact"} {
		cfg, ok := s.retryModels[consumer]
		if !ok || cfg.Endpoint == "" || cfg.Model == "" || cfg.CredentialFile == "" {
			continue
		}
		key, err := os.ReadFile(cfg.CredentialFile)
		if err != nil {
			logging.Warnf("model_retry_credential_unreadable")
			continue
		}
		fingerprint := backoff.Fingerprint(cfg.Endpoint, cfg.Model, cfg.Effort, cfg.Thinking, key)
		if _, err := s.retry.ObserveConfig(ctx, consumer, fingerprint); err != nil {
			return err
		}
	}
	s.retryReady = true
	return nil
}

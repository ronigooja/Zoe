package voice

import (
	"context"
	"os"
	"time"

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
	if s.recallCtx != nil {
		s.lifecycleMu.Lock()
		defer s.lifecycleMu.Unlock()
		if s.stopped {
			return context.Canceled
		}
	}
	for _, consumer := range []string{"register", "steward", "ui_controller", "compact"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		cfg, ok := s.retryModels[consumer]
		if !ok {
			continue
		}
		if cfg.Endpoint == "" || cfg.Model == "" || cfg.CredentialFile == "" {
			logging.Warnf("model_retry_config_unavailable")
			continue
		}
		key, err := os.ReadFile(cfg.CredentialFile)
		if err != nil {
			logging.Warnf("model_retry_credential_unreadable")
			continue
		}
		fingerprint := backoff.Fingerprint(cfg.Endpoint, cfg.Model, cfg.Effort, cfg.Thinking, key)
		if _, err := s.retry.ObserveConfig(ctx, consumer, fingerprint); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logging.Warnf("model_retry_observe_failed")
		}
	}
	if s.recallCtx != nil {
		s.startRetryPublisher()
	}
	s.retryReady = true
	return nil
}

func (s *Service) startRetryPublisher() {
	path := s.retryStatusPath
	if path == "" {
		path = backoff.StatusPath
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		publish := func() {
			if err := s.retry.Publish(s.recallCtx, path, time.Now()); err != nil && s.recallCtx.Err() == nil {
				logging.Warnf("model_retry_publish_failed")
			}
		}
		publish()
		for {
			select {
			case <-s.recallCtx.Done():
				return
			case <-s.retryPublish:
				timer := time.NewTimer(2 * time.Second)
				select {
				case <-s.recallCtx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				select {
				case <-s.retryPublish:
				default:
				}
				publish()
			}
		}
	}()
}

// PublishRetryState requests an asynchronous retry status publication.
func (s *Service) PublishRetryState() {
	if s.retryPublish != nil {
		select {
		case s.retryPublish <- struct{}{}:
		default:
		}
	}
}

// PublishRetryState requests an asynchronous retry status publication.
func (h *ReadyHandle) PublishRetryState() {
	if h != nil && h.service != nil {
		h.service.PublishRetryState()
	}
}

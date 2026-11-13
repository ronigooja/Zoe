package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memory"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func composeControlServer(store *storage.Store, controlConfig control.Config, voiceConfig voice.Config, capabilityConfig capabilities.Config, memoryConfig memory.DerivedConfig) (*control.Server, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ZOE_VOICE_ACCOUNT_MEDIA_ENABLED")), "true") {
		voiceConfig.ResultDeliveryEnabled = true
		adapter, err := voice.NewSpaceAccountMediaAdapter(voiceConfig.GatewayURL, voiceConfig.GatewayAPIKey, voiceConfig.HTTPClient)
		if err != nil {
			return nil, err
		}
		voiceConfig.AccountMediaLifecycle = adapter
	}
	server, err := control.NewServer(store, controlConfig)
	if err != nil {
		return nil, err
	}
	if err = memory.Register(server, store); err != nil {
		return nil, err
	}
	if err = memory.RegisterDerived(server, store, memoryConfig); err != nil {
		return nil, err
	}
	voiceConfig.ContextAssembler = server.ContextAssembler()
	voiceRepository := memory.NewRepositoryWithStore(store)
	if voiceConfig.RegisterConfig != nil {
		voiceConfig.RegisterConfig.MemoryBoundary = func(ctx context.Context, o voice.RegisterOwner) (time.Time, error) {
			return voiceRepository.BoundaryAt(ctx, o.WorkspaceID, o.OwnerSubjectID)
		}
	}
	voiceConfig.OutcomeWriter = func(ctx context.Context, p contracts.Principal, sessionID, summary, outcome string) error {
		return voiceRepository.RecordVoiceOutcome(ctx, p, sessionID, summary, outcome)
	}
	if err = voice.Register(server, store, voiceConfig); err != nil {
		return nil, err
	}
	if err = capabilities.Register(server, store, capabilityConfig); err != nil {
		return nil, err
	}
	if err = control.RegisterInstanceRoutes(server); err != nil {
		return nil, err
	}
	if err = control.RegisterExportRoutes(server); err != nil {
		return nil, err
	}
	return server, nil
}

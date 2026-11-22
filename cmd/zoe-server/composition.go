package main

import (
	"context"
	"os"
	"time"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memory"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func composeControlServer(store *storage.Store, controlConfig control.Config, voiceConfig voice.Config, capabilityConfig capabilities.Config) (*control.Server, error) {
	server, err := control.NewServer(store, controlConfig)
	if err != nil {
		return nil, err
	}
	if err = memoryapi.RegisterMigrations(store); err != nil {
		return nil, err
	}
	interval, err := memory.BatchInterval(os.Getenv("ZOE_MEMORY_BATCH_INTERVAL_HOURS"))
	if err != nil {
		return nil, err
	}
	voiceRepository := memory.NewRepositoryWithStore(store)
	if voiceConfig.Ready != nil {
		voiceRepository.OnRetryChange = voiceConfig.Ready.PublishRetryState
	}
	if voiceConfig.RegisterConfig != nil {
		voiceConfig.RegisterConfig.MemoryBoundary = func(ctx context.Context, o voice.RegisterOwner) (time.Time, error) {
			return voiceRepository.BoundaryAt(ctx, o.WorkspaceID, o.OwnerSubjectID)
		}
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
	if voiceConfig.MemoryAPI != nil {
		ctx, cancel := context.WithCancel(context.Background())
		server.RegisterShutdownHook(cancel)
		voiceRepository.StartBatches(ctx, voiceConfig.MemoryAPI, interval)
	}
	return server, nil
}

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
		assemble := func(ctx context.Context, o voice.RegisterOwner, query string) (contracts.MemoryContext, error) {
			p := contracts.Principal{WorkspaceID: o.WorkspaceID, SubjectID: o.OwnerSubjectID, Scopes: []string{"memory:read:personal"}}
			return voiceConfig.ContextAssembler(ctx, p, "", o.OwnerSubjectID, query, contracts.MemoryContextDefaultBudgetTokens)
		}
		voiceConfig.RegisterConfig.MemoryKeys = func(ctx context.Context, o voice.RegisterOwner) ([]voice.MemoryMappingKey, error) {
			rows, err := store.DB().QueryContext(ctx, `SELECT DISTINCT subject,claim_key FROM memory_claims WHERE workspace_id=? AND scope_kind='personal' AND principal_id=? AND status IN ('active','conflicted') AND deleted_at IS NULL ORDER BY subject,claim_key`, o.WorkspaceID, o.OwnerSubjectID)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var keys []voice.MemoryMappingKey
			for rows.Next() {
				var item voice.MemoryMappingKey
				if err := rows.Scan(&item.Subject, &item.Key); err != nil {
					return nil, err
				}
				keys = append(keys, item)
			}
			return keys, rows.Err()
		}
		voiceConfig.RegisterConfig.MemoryContent = func(ctx context.Context, o voice.RegisterOwner, key string) (string, error) {
			m, err := assemble(ctx, o, key)
			var text []string
			for _, item := range m.Items {
				if item.Key == key {
					text = append(text, item.Content)
				}
			}
			return strings.Join(text, "\n"), err
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

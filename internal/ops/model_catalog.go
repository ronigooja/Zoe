package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxOfficialCatalogSize = 16 << 20

func modelMetadataPath(o LifecycleOptions) string {
	return filepath.Join(openCodeRoot(o), "models.json")
}

func officialLimitMap(v map[string]any) map[string]ModelLimit {
	out := map[string]ModelLimit{}
	add := func(id string, model map[string]any) {
		if modelIDRE.MatchString(id) {
			if l, ok := model["limit"].(map[string]any); ok {
				out[id] = modelLimit(l)
			}
		}
	}
	if providers, ok := v["providers"].(map[string]any); ok {
		for _, p := range providers {
			if pm, ok := p.(map[string]any); ok {
				if models, ok := pm["models"].(map[string]any); ok {
					for id, model := range models {
						if mm, ok := model.(map[string]any); ok {
							add(id, mm)
						}
					}
				}
			}
		}
	}
	if models, ok := v["models"].(map[string]any); ok {
		for id, model := range models {
			if mm, ok := model.(map[string]any); ok {
				add(id, mm)
			}
		}
	}
	return out
}

func fetchOfficialLimits(ctx context.Context) (map[string]ModelLimit, error) {
	rawURL := os.Getenv("ZOE_OPENCODE_OFFICIAL_CATALOG_URL")
	if rawURL == "" {
		rawURL = "https://models.opencode.ai/catalog.json"
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid official catalog URL")
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != u.Host {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("official catalog HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxOfficialCatalogSize+1))
	if err != nil || len(b) > maxOfficialCatalogSize {
		return nil, fmt.Errorf("official catalog too large")
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return officialLimitMap(v), nil
}

func enrichProviderModels(ctx context.Context, result *ProbeResult) {
	official, err := fetchOfficialLimits(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: official model catalog unavailable; using gateway metadata only")
		official = nil
	}
	seen := map[string]bool{}
	out := make([]ProviderModelMetadata, 0, len(result.Models)+1)
	for _, m := range result.Models {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		l, ok := official[m.ID]
		if !ok {
			if i := strings.IndexByte(m.ID, '/'); i >= 0 {
				l, ok = official[m.ID[i+1:]]
			}
		}
		if ok {
			if m.Limit.Context == 0 {
				m.Limit.Context = l.Context
			}
			if m.Limit.Input == 0 {
				m.Limit.Input = l.Input
			}
			if m.Limit.Output == 0 {
				m.Limit.Output = l.Output
			}
		}
		out = append(out, m)
	}
	defaultID := result.Model
	if i := strings.IndexByte(defaultID, '/'); i >= 0 {
		defaultID = defaultID[i+1:]
	}
	if !seen[defaultID] && modelIDRE.MatchString(defaultID) {
		out = append(out, ProviderModelMetadata{ID: defaultID, Name: defaultID})
	}
	if len(out) > 500 {
		out = out[:500]
	}
	result.Models = out
}

func persistProviderModels(o LifecycleOptions, provider string, models []ProviderModelMetadata) error {
	data, err := json.MarshalIndent(map[string]any{"provider": provider, "models": models}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(modelMetadataPath(o), data, 0600)
}

func loadProviderModels(o LifecycleOptions, provider string) map[string]ProviderModelMetadata {
	b, err := os.ReadFile(modelMetadataPath(o))
	if err != nil {
		return nil
	}
	var v struct {
		Provider string                  `json:"provider"`
		Models   []ProviderModelMetadata `json:"models"`
	}
	if json.Unmarshal(b, &v) != nil || v.Provider != provider {
		return nil
	}
	out := map[string]ProviderModelMetadata{}
	for _, m := range v.Models {
		if modelIDRE.MatchString(m.ID) {
			out[m.ID] = m
		}
	}
	return out
}

func mergeProviderModels(o LifecycleOptions, provider, modelID string) map[string]any {
	meta := loadProviderModels(o, provider)
	models := map[string]any{}
	for id, m := range meta {
		entry := map[string]any{"name": m.Name}
		if entry["name"] == "" {
			entry["name"] = id
		}
		if m.Limit.Context > 0 || m.Limit.Input > 0 || m.Limit.Output > 0 {
			entry["limit"] = m.Limit
		}
		models[id] = entry
	}
	if _, ok := models[modelID]; !ok {
		models[modelID] = map[string]any{"name": modelID}
	}
	return models
}

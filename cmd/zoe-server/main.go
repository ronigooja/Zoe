// zoe-server starts the private control plane. Instance keys are provisioned
// using init-key with explicit secure output, or a deployment-provided secret.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ronigooja/Zoe/internal/steward"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ronigooja/Zoe/internal/answer"
	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memory"
	"github.com/ronigooja/Zoe/internal/modelclient"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/summarize"
	"github.com/ronigooja/Zoe/internal/voice"
)

func main() {
	if len(os.Args) > 1 {
		if err := manageInstanceKey(os.Args[1], os.Args[2:]); err != nil {
			log.Printf("instance key rotation failed: %v", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		log.Printf("zoe-server stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	addr := env("ZOE_LISTEN_ADDR", "127.0.0.1:8787")
	dbPath := strings.TrimSpace(os.Getenv("ZOE_DATABASE_PATH"))
	if dbPath == "" {
		return errors.New("ZOE_DATABASE_PATH is required")
	}
	profiles, err := loadProfiles(os.Getenv("ZOE_RUNTIME_PROFILES"))
	if err != nil {
		return err
	}
	memoryRunner, err := loadMemoryRunnerCredentialFromEnv()
	if err != nil {
		return err
	}
	voiceIngest, err := loadVoiceIngestCredentialFromEnv()
	if err != nil {
		return err
	}
	instanceKey := strings.TrimSpace(os.Getenv("ZOE_INSTANCE_KEY"))
	runnerToken := os.Getenv("ZOE_RUNNER_TOKEN")
	if runnerToken != "" && !validBearer(runnerToken) {
		return errors.New("ZOE_RUNNER_TOKEN must be a 32-byte base64url bearer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, storage.Config{Path: dbPath})
	if err != nil {
		return err
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		return err
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		return err
	}
	pushSender, err := configuredPushSender()
	if err != nil {
		return err
	}
	maxRuns := maxRunsEnv()
	if maxRuns == 0 {
		return errors.New("ZOE_MAX_RUNS must be an integer from 1 through 64")
	}
	waitLimit := waitLimitEnv()
	if waitLimit == 0 {
		return errors.New("ZOE_WAIT_LIMIT_MINUTES must be an integer from 1 through 1440")
	}
	cfg := control.Config{
		RunnerToken:        runnerToken,
		RunnerID:           env("ZOE_RUNNER_ID", "configured-runner"),
		RunnerWorkspaceIDs: splitEnv("ZOE_RUNNER_WORKSPACES", "default"),
		RunnerScopes:       splitEnv("ZOE_RUNNER_SCOPES", "runner:claim,runner:observe,runner:control:read"),
		InstanceScopes:     optionalSplitEnv("ZOE_INSTANCE_SCOPES"),
		DefaultProfileID:   strings.TrimSpace(os.Getenv("ZOE_DEFAULT_PROFILE_ID")),
		Profiles:           profiles,
		PushSender:         pushSender,
		MaxRuns:            maxRuns,
		WaitLimit:          waitLimit,
	}
	summaryClient, err := summarize.FromEnv(os.Getenv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("summary configuration: %w", err)
	}
	var summarizer voice.Summarizer
	if summaryClient != nil {
		summarizer = summaryClient
	}
	summarySettle, err := voice.SummarySettleDelay(os.Getenv("ZOE_SUMMARY_SETTLE_SECONDS"))
	if err != nil {
		return err
	}
	stewardClient, err := steward.FromEnv(os.Getenv, os.ReadFile)
	if err != nil {
		log.Fatalf("steward configuration: %v", err)
	}
	var taskSteward voice.TaskSteward
	if stewardClient != nil {
		taskSteward = stewardClient
	}
	answerClient, err := answer.FromEnv(os.Getenv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("answer configuration: %w", err)
	}
	answerTimeout, err := modelclient.Timeout("ZOE_ANSWER", os.Getenv, 8000, 500, 30000)
	if err != nil {
		return err
	}
	var answerer voice.Answerer
	classificationTimeout, err := modelclient.Timeout("ZOE_VOICE_CLASSIFICATION", os.Getenv, 35000, 100, 60000)
	if err != nil {
		return err
	}
	if answerClient != nil {
		answerer = answerClient
	}
	recallLimit := 20
	if raw := strings.TrimSpace(os.Getenv("ZOE_RECALL_SUMMARY_LIMIT")); raw != "" {
		recallLimit, err = strconv.Atoi(raw)
		if err != nil || recallLimit < 1 || recallLimit > 100 {
			return errors.New("ZOE_RECALL_SUMMARY_LIMIT must be between 1 and 100")
		}
	}
	registerConfig, err := registerConfigFromEnv(os.Getenv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("register configuration: %w", err)
	}
	server, err := composeControlServer(store, cfg, voice.Config{InstructionText: os.Getenv("ZOE_VOICE_INSTRUCTION_TEXT"), RegisterConfig: registerConfig, ClassificationTimeout: classificationTimeout, IngestEnabled: voiceIngest.Enabled, Steward: taskSteward, Summarizer: summarizer, SummarySettleDelay: &summarySettle, Answerer: answerer, RecallTimeout: answerTimeout, RecallSummaryLimit: recallLimit, RecallHoldingText: os.Getenv("ZOE_RECALL_HOLDING_TEXT"), GatewayURL: os.Getenv("ZOE_VOICE_GATEWAY_URL"), GatewayAPIKey: os.Getenv("ZOE_VOICE_GATEWAY_API_KEY"), UpstreamRoute: os.Getenv("ZOE_VOICE_UPSTREAM_ROUTE"), HeartbeatTimeout: voiceHeartbeatTimeout()}, capabilities.Config{
		AvailableCapabilityIDs: splitEnv("ZOE_AVAILABLE_CAPABILITY_IDS", ""),
		RuntimeBackend:         strings.TrimSpace(os.Getenv("ZOE_CAPABILITY_RUNTIME_BACKEND")),
	}, memory.DerivedConfig{BackupDeleteManifestPath: strings.TrimSpace(os.Getenv("ZOE_BACKUP_DELETE_MANIFEST_PATH"))})
	if err != nil {
		return err
	}
	if err = store.Migrate(ctx); err != nil {
		return err
	}
	server.StartRuntimeWaitingWorker()
	if _, _, err = control.InitializeInstanceAuthPending(ctx, store, instanceKey); err != nil {
		return err
	}
	var instanceKeyRows int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM instance_auth WHERE singleton=1`).Scan(&instanceKeyRows); err != nil {
		return err
	}
	if instanceKeyRows == 0 {
		log.Printf("instance key is pending; waiting for zoe key init")
	}
	if runnerToken != "" {
		if err = control.RegisterRunnerCredentialIfMissing(ctx, store, runnerToken, env("ZOE_RUNNER_ID", "configured-runner"), splitEnv("ZOE_RUNNER_WORKSPACES", "default"), splitEnv("ZOE_RUNNER_SCOPES", "runner:claim,runner:observe,runner:control:read")); err != nil {
			return err
		}
	}
	if memoryRunner.Enabled {
		if err = registerMemoryRunnerCredential(ctx, store, memoryRunner); err != nil {
			return err
		}
	}
	if voiceIngest.Enabled {
		if err = registerVoiceIngestCredential(ctx, store, voiceIngest); err != nil {
			return err
		}
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE streams are bounded by request context and shutdown.
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("zoe-server listening on %s", listener.Addr())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(listener)
	}()
	pushStop := make(chan struct{})
	if pushSender != nil {
		go runPushWorker(pushStop, server)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err = <-serveErr:
		server.Shutdown()
		close(pushStop)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signals:
		server.Shutdown()
		close(pushStop)
	}
	shutdown, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	if err = httpServer.Shutdown(shutdown); err != nil {
		_ = httpServer.Close()
		return err
	}
	if err = <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ZOE_VOICE_HEARTBEAT_TIMEOUT_SECONDS controls stale-session reclaim; values
// are clamped by the voice service to 30..600 seconds (default 60).
func voiceHeartbeatTimeout() time.Duration {
	value := strings.TrimSpace(os.Getenv("ZOE_VOICE_HEARTBEAT_TIMEOUT_SECONDS"))
	if value == "" {
		return 60 * time.Second
	}
	seconds, err := time.ParseDuration(value + "s")
	if err != nil {
		return 60 * time.Second
	}
	return seconds
}

func configuredPushSender() (control.PushSender, error) {
	envName := strings.TrimSpace(os.Getenv("ZOE_APNS_ENVIRONMENT"))
	teamID := strings.TrimSpace(os.Getenv("ZOE_APNS_TEAM_ID"))
	keyID := strings.TrimSpace(os.Getenv("ZOE_APNS_KEY_ID"))
	topic := strings.TrimSpace(os.Getenv("ZOE_APNS_TOPIC"))
	keyPath := strings.TrimSpace(os.Getenv("ZOE_APNS_PRIVATE_KEY_PATH"))
	configured := 0
	for _, value := range []string{envName, teamID, keyID, topic, keyPath} {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil, nil
	}
	if configured != 5 {
		return nil, errors.New("APNs configuration requires environment, team ID, key ID, topic, and private key path")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, errors.New("could not read deployment-provided APNs private key")
	}
	sender, err := control.NewAPNSSender(control.APNSConfig{Environment: envName, TeamID: teamID, KeyID: keyID, Topic: topic, PrivateKey: key})
	if err != nil {
		return nil, err
	}
	return sender, nil
}

func runPushWorker(stop <-chan struct{}, controlServer *control.Server) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			workCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			if err := controlServer.DispatchPushDeliveries(workCtx, 50); err != nil {
				log.Printf("push delivery worker failed: %v", err)
			}
			cancel()
		}
	}
}

func loadProfiles(raw string) (map[string]control.RuntimeProfile, error) {
	profiles := map[string]control.RuntimeProfile{}
	if strings.TrimSpace(raw) == "" {
		return profiles, nil
	}
	if err := json.Unmarshal([]byte(raw), &profiles); err != nil {
		return nil, errors.New("ZOE_RUNTIME_PROFILES must be a JSON object keyed by profile ID")
	}
	return profiles, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
func maxRunsEnv() int {
	v := strings.TrimSpace(os.Getenv("ZOE_MAX_RUNS"))
	if v == "" {
		return 12
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 64 {
		return 0
	}
	return n
}
func waitLimitEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv("ZOE_WAIT_LIMIT_MINUTES"))
	if v == "" {
		return 30 * time.Minute
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1440 {
		return 0
	}
	return time.Duration(n) * time.Minute
}

func splitEnv(name, fallback string) []string {
	value := env(name, fallback)
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func optionalSplitEnv(name string) []string {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func validBearer(value string) bool {
	bytes, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(bytes) == 32 && base64.RawURLEncoding.EncodeToString(bytes) == value
}

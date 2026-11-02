// zoe-runner executes server-authorized runs through OpenCode. Credentials and
// route configuration are deployment-provided environment values.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/runner"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("zoe-runner stopped: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("zoe-runner", flag.ContinueOnError)
	var preexistingContainers []string
	flags.Func("unrelated-preexisting-capability-container", "explicit full ID of a capability container predating the original run (repeatable)", func(value string) error {
		if !contracts.ValidFullContainerID(value) {
			return errors.New("preexisting capability requires a full lowercase Docker ID")
		}
		preexistingContainers = append(preexistingContainers, value)
		return nil
	})
	var retainContainers []string
	flags.Func("retain-never-started-container", "explicit full Docker ID to preserve after two complete never-started inspections (repeatable)", func(value string) error {
		if !contracts.ValidFullContainerID(value) {
			return errors.New("retained container requires a full lowercase Docker ID")
		}
		retainContainers = append(retainContainers, value)
		return nil
	})
	reconcileRuns := flags.String("reconcile-runs", "", "explicit comma-separated effect_unknown run IDs for read-only no-effect reconciliation")
	actor := flags.String("actor", "", "operator identity recorded in reconciliation proof")
	confirm := flags.Bool("confirm-no-effect", false, "confirm explicit no-effect reconciliation after complete runtime and Docker absence checks")
	once := flags.Bool("once", false, "reconcile current runs and perform one claim poll")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: zoe-runner [--once]")
	}
	baseURL := requiredEnv("ZOE_CONTROL_URL")
	controlToken := requiredEnv("ZOE_RUNNER_TOKEN")
	opencodeModel := requiredEnv("ZOE_OPENCODE_MODEL")
	opencodeURL := requiredEnv("ZOE_OPENCODE_BASE_URL")
	opencodePasswordFile := requiredEnv("ZOE_OPENCODE_PASSWORD_FILE")
	workspaceRoot := requiredEnv("ZOE_OPENCODE_WORKSPACE_ROOT")
	journalPath := requiredEnv("ZOE_RUNNER_JOURNAL_PATH")
	routes, err := loadRoutes(os.Getenv("ZOE_RUNNER_ROUTES"))
	if err != nil {
		return err
	}
	control, err := runner.NewControlClient(baseURL, controlToken, nil)
	if err != nil {
		return fmt.Errorf("configure control API: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	deploymentLock, err := runner.OpenDeploymentHostLock(runner.DeploymentHostLockPath)
	if err != nil {
		return err
	}
	defer deploymentLock.Close()
	deploymentAuthority, err := runner.OpenDeploymentAuthority(ctx, runner.DeploymentAuthorityPath)
	if err != nil {
		return err
	}
	defer deploymentAuthority.Close()
	journal, err := runner.OpenJournal(ctx, journalPath)
	if err != nil {
		return err
	}
	defer journal.Close()
	docker := runner.ExecDockerCommand{Binary: optionalEnv("ZOE_DOCKER_BINARY", "docker")}
	password, err := os.ReadFile(opencodePasswordFile)
	if opencodeURL == "" || opencodeModel == "" || opencodePasswordFile == "" || workspaceRoot == "" || err != nil || strings.TrimSpace(string(password)) == "" {
		return errors.New("ZOE_OPENCODE_BASE_URL, ZOE_OPENCODE_PASSWORD_FILE, ZOE_OPENCODE_MODEL, and ZOE_OPENCODE_WORKSPACE_ROOT are required")
	}
	openCode := runner.OpenCodeRuntime{BaseURL: opencodeURL, Username: "opencode", Password: strings.TrimSpace(string(password)), WorkspaceRoot: workspaceRoot, Model: opencodeModel}
	engine := &runner.Engine{
		Control:        control,
		Instance:       control,
		Runtime:        openCode,
		Journal:        journal,
		Routes:         routes,
		Isolation:      nil,
		PollInterval:   durationEnv("ZOE_RUNNER_POLL_INTERVAL", 2*time.Second),
		ObserveTimeout: durationEnv("ZOE_RUNNER_OBSERVE_TIMEOUT", 15*time.Second),
		WaitLimit:      durationEnv("ZOE_WAIT_LIMIT", 30*time.Minute),
	}
	if *reconcileRuns != "" || *confirm || *actor != "" || len(retainContainers) > 0 || len(preexistingContainers) > 0 {
		if *once {
			return errors.New("--once cannot be combined with explicit reconciliation")
		}
		return engine.ReconcileNoEffect(ctx, strings.Split(*reconcileRuns, ","), *actor, *confirm, nil, docker, runner.ReconciliationOptions{UnrelatedPreexistingCapabilityContainerIDs: preexistingContainers, RetainNeverStartedContainerIDs: retainContainers, DeploymentAuthority: deploymentAuthority})
	}
	deployments := &runner.DeploymentWorker{Control: control, Journal: journal, Executor: &runner.DeploymentExecutor{Docker: docker, Fences: deploymentAuthority}}
	var triggerAdmissions *runner.TriggerAdmissionWorker
	if boolEnv("ZOE_RUNNER_CAPABILITY_TRIGGERS_ENABLED", false) {
		triggerAdmissions = &runner.TriggerAdmissionWorker{Control: control, OnProgramUnsupported: func(count int) {
			log.Printf("%d capability program trigger(s) remain queued; program_trigger_executor_unavailable", count)
		}}
	}
	return serve(ctx, engine, deployments, triggerAdmissions, *once, durationEnv("ZOE_RUNNER_IDLE_POLL_INTERVAL", 2*time.Second))
}

func serve(ctx context.Context, engine *runner.Engine, deployments *runner.DeploymentWorker, triggerAdmissions *runner.TriggerAdmissionWorker, once bool, idle time.Duration) error {
	// Current claims include work already assigned to this runner. Replay those
	// exact journaled requests before accepting another claim after restart.
	current, err := engine.Control.Current(ctx)
	if err != nil {
		return fmt.Errorf("reconcile current claims: %w", err)
	}
	if once && triggerAdmissions != nil {
		if err := triggerAdmissions.Run(ctx, true, durationEnv("ZOE_RUNNER_TRIGGER_POLL_INTERVAL", 15*time.Second)); err != nil {
			return fmt.Errorf("admit capability triggers: %w", err)
		}
	}
	if once && deployments != nil {
		if err := deployments.Run(ctx, true, idle); err != nil && !errors.Is(err, runner.ErrNoDeployment) {
			return err
		}
	}
	var wg sync.WaitGroup
	var deploymentDone chan struct{}
	var triggerAdmissionDone chan struct{}
	if !once && deployments != nil {
		deploymentDone = make(chan struct{})
		go func() {
			defer close(deploymentDone)
			if err := deployments.Run(ctx, false, idle); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("deployment worker polling failed: %v", err)
			}
		}()
	}
	if !once && triggerAdmissions != nil {
		triggerAdmissionDone = make(chan struct{})
		go func() {
			defer close(triggerAdmissionDone)
			if err := triggerAdmissions.Run(ctx, false, durationEnv("ZOE_RUNNER_TRIGGER_POLL_INTERVAL", 15*time.Second)); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("capability trigger admission polling failed: %v", err)
			}
		}()
	}
	shutdown := func() {
		wg.Wait()
		if deploymentDone != nil {
			<-deploymentDone
		}
		if triggerAdmissionDone != nil {
			<-triggerAdmissionDone
		}
	}
	active := make(chan struct{}, 4)
	start := func(claim contracts.JobClaim, recovery bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-active }()
			if err := engine.Process(ctx, claim); err != nil && !errors.Is(err, runner.ErrOutcomePending) {
				label := "runner iteration failed"
				if recovery {
					label = "run reconciliation remains pending"
				}
				log.Printf("%s for run %s: %v", label, safeID(claim.RunID), err)
			}
		}()
	}
	for _, claim := range current {
		if len(active) == cap(active) {
			log.Printf("more current claims than the runner's four-run local limit; leaving run %s for another runner", safeID(claim.RunID))
			continue
		}
		select {
		case active <- struct{}{}:
			start(claim, true)
		case <-ctx.Done():
			shutdown()
			return nil
		}
	}
	if once {
		select {
		case active <- struct{}{}:
			err := engine.RunOnce(ctx)
			<-active
			shutdown()
			if errors.Is(err, runner.ErrNoClaim) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdown()
			return nil
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			shutdown()
			return nil
		}
		select {
		case active <- struct{}{}:
		case <-ctx.Done():
			shutdown()
			return nil
		}
		claim, err := engine.Control.Claim(ctx)
		switch {
		case err == nil:
			if claim == nil {
				<-active
			} else {
				start(*claim, false)
			}
		case errors.Is(err, runner.ErrNoClaim):
			<-active
		case errors.Is(err, runner.ErrOutcomePending):
			<-active
		default:
			<-active
			log.Printf("runner iteration failed; retrying after idle interval: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(idle):
		}
	}
}

func loadRoutes(raw string) (map[string]runner.Route, error) {
	routes := map[string]runner.Route{}
	if strings.TrimSpace(raw) == "" {
		return routes, errors.New("ZOE_RUNNER_ROUTES is required; it must match server-selected profiles")
	}
	if err := json.Unmarshal([]byte(raw), &routes); err != nil || len(routes) == 0 {
		return nil, errors.New("ZOE_RUNNER_ROUTES must be a non-empty JSON object keyed by profile ID")
	}
	for id, route := range routes {
		if id == "" || route.ProfileID != id || route.RuntimeRouteID == "" || route.ModelRouteID == "" || route.IsolationPolicy == "" || route.Harness == "" || route.Model == "" {
			return nil, fmt.Errorf("runner route %q is incomplete or has a mismatched profile id", id)
		}
	}
	return routes, nil
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return ""
	}
	return value
}

func optionalEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}

func boolEnv(name string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes"
}

func safeID(id string) string {
	if len(id) > 64 {
		return id[:64]
	}
	return id
}

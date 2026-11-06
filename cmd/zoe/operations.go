package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/ops"
	"github.com/skip2/go-qrcode"
)

func init() {
	_ = RegisterOperation("setup", setupOperation)
	_ = RegisterOperation("provider configure", providerConfigureOperation)
	_ = RegisterOperation("provider test", providerTestOperation)
	_ = RegisterOperation("memory configure", memoryConfigureOperation)
	_ = RegisterOperation("memory test", memoryTestOperation)
	_ = RegisterOperation("triage configure", triageConfigureOperation)
	_ = RegisterOperation("triage test", triageTestOperation)
	_ = RegisterOperation("key init", keyInitOperation)
	_ = RegisterOperation("key rotate", keyRotateOperation)
	_ = RegisterOperation("status", statusOperation)
	_ = RegisterOperation("install", installOperation)
	_ = RegisterOperation("update", updateOperation)
	_ = RegisterOperation("uninstall", uninstallOperation)
	_ = RegisterOperation("client-policy show", clientPolicyShowOperation)
	_ = RegisterOperation("client-policy set", clientPolicySetOperation)
}

func clientPolicyMaintenance(args []string, set bool, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("client-policy", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	db := fs.String("database", os.Getenv("ZOE_DATABASE_PATH"), "control database path")
	level := fs.String("level", "", "client log level: error, info, or debug")
	interval := fs.Int("interval", 0, "client poll interval in seconds")
	offline := fs.Bool("offline", false, "skip managed service pause; require exclusive database lock")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if set && !validPolicyLevel(*level) {
		return errors.New("--level must be error, info, or debug")
	}
	if *db == "" {
		d, err := ops.DiscoverDeployment(context.Background(), opsManager(), *root)
		if err != nil {
			return err
		}
		*db = d.DatabasePath
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	resume := func() error { return nil }
	if !*offline {
		resume, err = pauseControl(ctx, *root)
		if err != nil {
			return err
		}
		defer func() {
			if resumeErr := resume(); resumeErr != nil {
				fmt.Fprintln(errOut, "Service recovery failed; restart the control service manually.")
			}
		}()
	}
	var policy control.ClientPolicy
	if set {
		policy, err = ops.UpdateClientPolicy(ctx, ops.ClientPolicyOptions{DatabasePath: *db}, *level, *interval)
	} else {
		policy, err = ops.ReadClientPolicy(ctx, ops.ClientPolicyOptions{DatabasePath: *db})
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Log level: %s\nPoll interval: %d seconds\nRevision: %d\n", safeLabel(policy.LogLevel), policy.PollIntervalSeconds, policy.Revision)
	return nil
}

func clientPolicyShowOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	return clientPolicyMaintenance(args, false, out, errOut)
}
func clientPolicySetOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	return clientPolicyMaintenance(args, true, out, errOut)
}
func validPolicyLevel(level string) bool {
	return level == "error" || level == "info" || level == "debug"
}

func lifecycleFlags(name string, args []string, errOut io.Writer) (ops.LifecycleOptions, *flag.FlagSet, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	o := ops.LifecycleOptions{Out: errOut}
	fs.StringVar(&o.Release, "release", "", "absolute extracted release directory")
	fs.StringVar(&o.Root, "root", "/etc/zoe", "deployment configuration root")
	fs.StringVar(&o.OptRoot, "opt-root", "/opt/zoe", "release installation root")
	fs.StringVar(&o.DataRoot, "data-root", "/var/lib/zoe", "persistent data root")
	fs.StringVar(&o.OpenCodeImage, "opencode-image", "", "pinned GHCR OpenCode image")
	fs.StringVar(&o.OpenCodeBinary, "opencode-binary", "", "path to the host-installed OpenCode executable")
	fs.BoolVar(&o.NonInteractive, "non-interactive", false, "do not prompt")
	fs.BoolVar(&o.Yes, "yes", false, "confirm destructive operation")
	fs.BoolVar(&o.Yes, "non-interactive-with-yes", false, "confirm destructive operation without prompts")
	fs.BoolVar(&o.PurgeData, "purge-data", false, "remove Zoe data")
	fs.BoolVar(&o.PurgeCredentials, "purge-credentials", false, "remove protected gateway credentials")
	fs.BoolVar(&o.Start, "start", false, "start services and wait for health after installation")
	if err := fs.Parse(args); err != nil {
		return o, fs, err
	}
	if fs.NArg() != 0 {
		return o, fs, errors.New("unexpected arguments")
	}
	return o, fs, nil
}
func releasePrompt(o *ops.LifecycleOptions, in io.Reader, out io.Writer) error {
	if o.Release != "" {
		return nil
	}
	if o.NonInteractive {
		return errors.New("--release is required")
	}
	fmt.Fprint(out, "Release directory: ")
	v, e := inputLine(in)
	if e != nil {
		return e
	}
	o.Release = strings.TrimSpace(v)
	return nil
}
func installOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	o, _, e := lifecycleFlags("install", args, errOut)
	if e != nil {
		return e
	}
	if e = releasePrompt(&o, in, out); e != nil {
		return e
	}
	fmt.Fprintln(out, "Installing the reviewed Zoe host release; services start before key and model configuration.")
	_, e = opsManager().Install(context.Background(), o)
	return e
}
func updateOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	o, _, e := lifecycleFlags("update", args, errOut)
	if e != nil {
		return e
	}
	if e = releasePrompt(&o, in, out); e != nil {
		return e
	}
	fmt.Fprintln(out, "Updating Zoe atomically; the previous release is retained until health checks pass.")
	return opsManager().Update(context.Background(), o)
}
func uninstallOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	o, _, e := lifecycleFlags("uninstall", args, errOut)
	if e != nil {
		return e
	}
	if o.NonInteractive && !o.Yes {
		return errors.New("uninstall requires --yes in non-interactive mode")
	}
	plan, err := opsManager().UninstallPlan(context.Background(), o)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Uninstall plan:")
	fmt.Fprintf(out, "  units: %s\n", strings.Join(plan.Units, ", "))
	fmt.Fprintf(out, "  Compose file: %s\n", safeLabel(plan.ComposeFile))
	fmt.Fprintf(out, "  Compose containers: %s\n", strings.Join(plan.ComposeContainers, ", "))
	fmt.Fprintf(out, "  OpenCode containers: %s\n", strings.Join(plan.RuntimeContainers, ", "))
	fmt.Fprintf(out, "  Zoe image IDs: %s\n", strings.Join(plan.ImageIDs, ", "))
	fmt.Fprintf(out, "  network: %s\n  release root: %s\n", plan.Network, plan.OptRoot)
	fmt.Fprintln(out, "  removes /opt/zoe including the installed CLI and all releases; a later zoe install needs a fresh operator package")
	if o.PurgeData {
		fmt.Fprintf(out, "  purge data: %s\n  purge non-credential /etc/zoe files\n", plan.DataRoot)
		fmt.Fprintf(out, "  keep credentials: %s\n", strings.Join(plan.KeepFiles, ", "))
	}
	if !o.Yes {
		fmt.Fprint(out, "Type yes to continue: ")
		v, x := inputLine(in)
		if x != nil {
			return x
		}
		if strings.TrimSpace(v) != "yes" {
			return errors.New("confirmation required")
		}
	}
	fmt.Fprintln(out, "Removing the listed Zoe host resources.")
	return opsManager().Uninstall(context.Background(), o)
}

func setupOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	fmt.Fprintln(out, "Setup checks the installed deployment and configures its task model.")
	if err := providerConfigureOperation(args, in, out, errOut); err != nil {
		return err
	}
	root := setupRoot(args)
	fmt.Fprintln(out, "Now configure the memory model. Press Enter at the URL prompt to skip this step without changing anything.")
	memoryArgs := []string{"--root", root}
	for _, arg := range args {
		if arg == "--non-interactive" {
			memoryArgs = append(memoryArgs, arg)
		}
	}
	return memoryConfigureOperationWithSkip(memoryArgs, in, out, errOut, true)
}

func setupRoot(args []string) string {
	for i, arg := range args {
		if arg == "--root" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--root=") {
			return strings.TrimPrefix(arg, "--root=")
		}
	}
	return "/etc/zoe"
}

// providerFlags are shared by setup, configure, test and status so the same
// deployment is used by interactive and scripted operations.
type providerFlags struct {
	root, base, model, secret, token *string
}

func addProviderFlags(fs *flag.FlagSet, token bool) providerFlags {
	f := providerFlags{root: fs.String("root", "", "deployment configuration root (default /etc/zoe)"), base: fs.String("url", "", "override provider URL"), model: fs.String("model", "", "override model"), secret: fs.String("secret-file", "", "override private token file")}
	if token {
		f.token = fs.String("token", "", "compatibility token input; shell history and process arguments may retain it (prefer prompt)")
	}
	return f
}

var opsManager = func() ops.Manager { return ops.NewManager(nil) }

func providerDeployment(ctx context.Context, f providerFlags) (ops.Deployment, ops.ProviderConfig, error) {
	d, err := ops.DiscoverDeployment(ctx, opsManager(), *f.root)
	if err != nil {
		return d, ops.ProviderConfig{}, err
	}
	cfg, err := d.ProviderConfig()
	if err != nil {
		return d, cfg, err
	}
	if *f.base != "" {
		cfg.BaseURL = *f.base
	}
	if *f.model != "" {
		cfg.Model = *f.model
	}
	if *f.secret != "" {
		cfg.SecretPath = *f.secret
	}
	return d, cfg, nil
}
func providerConfigureOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("provider configure", flag.ContinueOnError)
	fs.SetOutput(errOut)
	f := addProviderFlags(fs, true)
	nonInteractive := fs.Bool("non-interactive", false, "require all provider values without prompts")
	tokenFile := fs.String("token-file", "", "read a private token file (preferred for scripts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	d, cfg, err := providerDeployment(ctx, f)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Deployment: %s\nDatabase: %s\nRunner config: %s\nRunner service: %s\n", d.Mode, d.DatabasePath, d.RunnerEnv, d.RunnerService)
	in = sharedInput(in)
	prompt := func(label string, value *string) error {
		if *nonInteractive {
			if strings.TrimSpace(*value) != "" {
				return nil
			}
			return fmt.Errorf("%s is required", label)
		}
		if strings.TrimSpace(*value) != "" {
			fmt.Fprintf(out, "%s [%s]: ", label, safeLabel(*value))
		} else {
			fmt.Fprintf(out, "%s: ", label)
		}
		v, err := inputLine(in)
		if err != nil {
			return err
		}
		if strings.TrimSpace(v) != "" {
			*value = v
		}
		return nil
	}
	if err := prompt("CLIProxyAPI URL", &cfg.BaseURL); err != nil {
		return err
	}
	if err := prompt("Model", &cfg.Model); err != nil {
		return err
	}
	if *f.token != "" && *tokenFile != "" {
		return errors.New("use only one of --token and --token-file")
	}
	secret := []byte(*f.token)
	if *tokenFile != "" {
		secret, err = os.ReadFile(*tokenFile)
		if err != nil {
			return errors.New("cannot read token file")
		}
	}
	if len(secret) == 0 {
		if *nonInteractive {
			return errors.New("--token-file or --token is required for non-interactive configure")
		}
		secret, err = readToken(in, out)
		if err != nil {
			return err
		}
	}
	cfg.Secret = strings.TrimSpace(string(secret))
	if _, err := opsManager().ConfigureProvider(ctx, d, cfg, nil); err != nil {
		fmt.Fprintf(errOut, "Provider configuration failed: %s\n", safeLabel(err.Error()))
		return err
	}
	fmt.Fprintln(out, "Provider configured, checked and services reloaded.")
	return nil
}
func providerTestOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("provider test", flag.ContinueOnError)
	fs.SetOutput(errOut)
	f := addProviderFlags(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d, cfg, err := providerDeployment(ctx, f)
	if err != nil {
		return err
	}
	result, err := opsManager().TestProvider(ctx, d, cfg, nil)
	fmt.Fprintf(out, "HTTP status: %d\nModel available: %t\nResult: %s\n", result.HTTPStatus, result.ModelAvailable, safeLabel(result.ErrorCategory))
	if err != nil {
		fmt.Fprintf(errOut, "Provider test failed: %s\n", safeLabel(err.Error()))
	}
	return err
}

func memoryConfigureOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	return memoryConfigureOperationWithSkip(args, in, out, errOut, false)
}

func memoryConfigureOperationWithSkip(args []string, in io.Reader, out, errOut io.Writer, allowSkip bool) error {
	fs := flag.NewFlagSet("memory configure", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	base := fs.String("url", "", "override memory model gateway URL")
	baseAlias := fs.String("base-url", "", "override memory model gateway URL")
	model := fs.String("model", "", "override memory model")
	effort := fs.String("effort", "", "override reasoning effort (none, low, medium, high, max, or empty)")
	secretPath := fs.String("secret", ops.DefaultMemorySecretPath, "private memory credential file")
	secretFileAlias := fs.String("secret-file", "", "compatibility alias for --secret")
	token := fs.String("token", "", "compatibility token input; shell history and process arguments may retain it (prefer prompt)")
	tokenFile := fs.String("token-file", "", "read a private memory credential file")
	keyFile := fs.String("key-file", "", "record an existing private memory key file")
	keyStdin := fs.Bool("key-stdin", false, "read the private memory key from stdin")
	disable := fs.Bool("disable", false, "disable memory model")
	nonInteractive := fs.Bool("non-interactive", false, "require all memory values without prompts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *keyFile != "" {
		if *tokenFile != "" {
			return errors.New("use only one of --key-file and --token-file")
		}
		*tokenFile = *keyFile
	}
	if *keyStdin && (*tokenFile != "" || *token != "") {
		return errors.New("use only one key input")
	}
	if *secretFileAlias != "" {
		if *secretPath != ops.DefaultMemorySecretPath {
			return errors.New("use only one of --secret and --secret-file")
		}
		*secretPath = *secretFileAlias
	}
	if *base == "" {
		*base = *baseAlias
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	d, err := ops.DiscoverDeployment(ctx, opsManager(), *root)
	if err != nil {
		return err
	}
	cfg, err := d.MemoryConfig()
	if err != nil {
		return err
	}
	if *base != "" {
		cfg.BaseURL = *base
	}
	if *model != "" {
		cfg.Model = *model
	}
	if hasFlag(args, "--effort") {
		cfg.ReasoningEffort = *effort
	}
	cfg.SecretPath = *secretPath
	if *disable {
		cfg.BaseURL = ""
		cfg.Model = ""
		if err := ops.WriteMemoryEnvAtomic(d.StackEnv, cfg); err != nil {
			return err
		}
		fmt.Fprintln(out, "Memory model disabled.")
		fmt.Fprintln(out, "Apply with: zoe update")
		return nil
	}
	provider, err := d.ProviderConfig()
	if err != nil {
		return err
	}
	in = sharedInput(in)
	if allowSkip {
		fmt.Fprint(out, "Memory model gateway URL (press Enter to skip): ")
		value, readErr := inputLine(in)
		if readErr != nil {
			return readErr
		}
		if strings.TrimSpace(value) == "" {
			fmt.Fprintln(out, "Memory model configuration skipped; existing memory settings were not changed.")
			return nil
		}
		cfg.BaseURL = value
	} else if strings.TrimSpace(cfg.BaseURL) == "" {
		if *nonInteractive {
			return errors.New("--url is required")
		}
		fmt.Fprint(out, "Memory model gateway URL: ")
		cfg.BaseURL, err = inputLine(in)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.Model) == "" {
		if *nonInteractive {
			return errors.New("--model is required")
		}
		fmt.Fprint(out, "Memory model: ")
		cfg.Model, err = inputLine(in)
		if err != nil {
			return err
		}
	}
	if err := ops.ValidateReasoningEffort(cfg.ReasoningEffort); err != nil {
		return err
	}
	if *token != "" && *tokenFile != "" {
		return errors.New("use only one of --token and --token-file")
	}
	if *token != "" {
		cfg.Secret = strings.TrimSpace(*token)
	} else if *keyStdin {
		secret, readErr := io.ReadAll(in)
		if readErr != nil {
			return errors.New("cannot read key from stdin")
		}
		cfg.Secret = strings.TrimSpace(string(secret))
		for i := range secret {
			secret[i] = 0
		}
	} else if *tokenFile == "" {
		if *nonInteractive {
			return errors.New("--token-file or --token is required for non-interactive configure")
		}
		secret, readErr := readToken(in, out)
		if readErr != nil {
			return readErr
		}
		cfg.Secret = strings.TrimSpace(string(secret))
		for i := range secret {
			secret[i] = 0
		}
	} else {
		secret, readErr := os.ReadFile(*tokenFile)
		if readErr != nil {
			return errors.New("cannot read memory token file")
		}
		cfg.Secret = strings.TrimSpace(string(secret))
		for i := range secret {
			secret[i] = 0
		}
	}
	if err := ops.ValidateReasoningEffort(cfg.ReasoningEffort); err != nil {
		return err
	}
	if *tokenFile != "" && *keyStdin == false {
		if err := ops.ValidateKeyFile(*tokenFile); err != nil {
			return err
		}
	}
	if filepath.Clean(cfg.SecretPath) == filepath.Clean(provider.SecretPath) {
		return errors.New("memory secret path must be distinct from the task-model secret path")
	}
	result, err := opsManager().ConfigureMemory(ctx, d, cfg, provider.SecretPath, nil)
	fmt.Fprintf(out, "HTTP status: %d\nModel available: %t\nResult: %s\n", result.HTTPStatus, result.ModelAvailable, safeLabel(result.ErrorCategory))
	if err != nil {
		fmt.Fprintf(errOut, "Memory configuration failed: %s\n", safeLabel(err.Error()))
		return err
	}
	fmt.Fprintln(out, "Memory model configured, checked and memory worker reloaded.")
	return nil
}

func triageConfigureOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("triage configure", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	base := fs.String("base-url", "", "triage model base URL")
	legacyURL := fs.String("url", "", "triage model base URL")
	model := fs.String("model", "", "triage model")
	effort := fs.String("effort", "", "reasoning effort (none, low, medium, high, max, or empty)")
	thinking := fs.String("thinking", "", "thinking mode (enabled, disabled, or empty)")
	keyFile := fs.String("key-file", "", "existing private triage key file")
	secretFile := fs.String("secret-file", "", "private triage key destination")
	keyStdin := fs.Bool("key-stdin", false, "read the private triage key from stdin")
	disable := fs.Bool("disable", false, "disable triage without deleting other values")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *base == "" {
		*base = *legacyURL
	}
	if *keyFile != "" && *secretFile != "" {
		return errors.New("use only one of --key-file and --secret-file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := ops.DiscoverDeployment(ctx, opsManager(), *root)
	if err != nil {
		return err
	}
	cfg, err := d.TriageConfig()
	if err != nil {
		return err
	}
	if *base != "" {
		cfg.BaseURL = *base
	}
	if *model != "" {
		cfg.Model = *model
	}
	if hasFlag(args, "--effort") {
		cfg.ReasoningEffort = *effort
	}
	if hasFlag(args, "--thinking") {
		cfg.Thinking = *thinking
		if cfg.Thinking != "" && cfg.Thinking != "enabled" && cfg.Thinking != "disabled" {
			return errors.New("triage thinking must be enabled, disabled, or empty")
		}
	}
	if *keyFile != "" {
		cfg.SecretPath = *keyFile
	}
	if *secretFile != "" {
		cfg.SecretPath = *secretFile
	}
	if cfg.SecretPath == "" {
		cfg.SecretPath = filepath.Join(*root, "secrets", "triage-api-key")
	}
	if *disable {
		if err := ops.WriteTriageEnvAtomic(d.StackEnv, cfg, true); err != nil {
			return err
		}
		fmt.Fprintln(out, "Triage model disabled.")
		fmt.Fprintln(out, "Apply with: zoe update")
		return nil
	}
	if err := ops.ValidateTriageBaseURL(cfg.BaseURL, ""); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return errors.New("triage model is required")
	}
	if err := ops.ValidateReasoningEffort(cfg.ReasoningEffort); err != nil {
		return err
	}
	if *keyStdin {
		secret, readErr := io.ReadAll(in)
		if readErr != nil {
			return errors.New("cannot read key from stdin")
		}
		defer func() {
			for i := range secret {
				secret[i] = 0
			}
		}()
		if err := ops.WriteTriageSecretAtomic(cfg.SecretPath, secret); err != nil {
			return err
		}
	} else {
		if err := ops.ValidateKeyFile(cfg.SecretPath); err != nil {
			return err
		}
	}
	if err := ops.WriteTriageEnvAtomic(d.StackEnv, cfg, false); err != nil {
		return err
	}
	fmt.Fprintf(out, "Triage model configured: model=%s base-url-host=%s key-file-present=yes\n", safeLabel(cfg.Model), safeHost(cfg.BaseURL))
	fmt.Fprintln(out, "Apply with: zoe update")
	return nil
}

func triageTestOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("triage test", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	text := fs.String("text", "好了吗", "spoken sentence to classify")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := ops.DiscoverDeployment(ctx, opsManager(), *root)
	if err != nil {
		return err
	}
	cfg, err := d.TriageConfig()
	if err != nil {
		return err
	}
	r, testErr := ops.TestTriage(ctx, cfg, nil, *text)
	class := "FAIL"
	if testErr == nil {
		class = "PASS"
	}
	fmt.Fprintf(out, "%s style=%s HTTP %dxx latency=%dms category=%s\n", class, safeLabel(r.Style), r.HTTPStatus/100, r.Latency.Milliseconds(), safeLabel(r.Category))
	if testErr != nil {
		fmt.Fprintln(errOut, safeLabel(testErr.Error()))
	}
	return testErr
}

func safeHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func memoryTestOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("memory test", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d, err := ops.DiscoverDeployment(ctx, opsManager(), *root)
	if err != nil {
		return err
	}
	cfg, err := d.MemoryConfig()
	if err != nil {
		return err
	}
	result, err := opsManager().TestMemory(ctx, d, cfg, nil)
	fmt.Fprintf(out, "HTTP status: %d\nModel available: %t\nResult: %s\n", result.HTTPStatus, result.ModelAvailable, safeLabel(result.ErrorCategory))
	if err != nil {
		fmt.Fprintf(errOut, "Memory test failed: %s\n", safeLabel(err.Error()))
	}
	return err
}

func keyInitOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	return keyOperation(false, args, in, out, errOut)
}
func keyRotateOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	return keyOperation(true, args, in, out, errOut)
}

// secretTerminal is separate from stdout so shell redirection never captures
// a newly generated credential or QR. Tests can replace this boundary.
var secretTerminal = func() (io.WriteCloser, error) { return os.OpenFile("/dev/tty", os.O_WRONLY, 0) }

func keyOperation(rotate bool, args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	db := fs.String("database", os.Getenv("ZOE_DATABASE_PATH"), "control database path")
	server := fs.String("server", os.Getenv("ZOE_SERVER_URL"), "required server URL for app connection")
	output := fs.String("output", "", "private 0600 JSON export (default beside database)")
	nonInteractive := fs.Bool("non-interactive", false, "require server URL without a prompt")
	confirm := fs.Bool("yes", false, "confirm immediate invalidation of the old key on rotation")
	offline := fs.Bool("offline", false, "skip managed service pause; require exclusive database lock")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if *db == "" {
		d, err := ops.DiscoverDeployment(context.Background(), opsManager(), *root)
		if err != nil {
			return err
		}
		*db = d.DatabasePath
	}
	in = sharedInput(in)
	if strings.TrimSpace(*server) == "" {
		if *nonInteractive {
			return errors.New("--server is required")
		}
		fmt.Fprint(out, "Zoe server URL: ")
		value, err := inputLine(in)
		if err != nil {
			return err
		}
		*server = value
	}
	if err := ops.ValidateServerURL(*server); err != nil {
		return err
	}
	if rotate {
		fmt.Fprintln(out, "Rotating immediately invalidates the old instance key on every connected app.")
		if !*confirm {
			if *nonInteractive {
				return errors.New("rotation requires --yes")
			}
			fmt.Fprint(out, "Rotate key now? [y/N]: ")
			answer, err := inputLine(in)
			if err != nil {
				return err
			}
			if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
				return errors.New("rotation cancelled")
			}
		}
	}
	if *output == "" {
		*output = filepath.Join(filepath.Dir(*db), "instance-key-"+time.Now().UTC().Format("20060102T150405.000000000")+".json")
	}
	fmt.Fprintln(out, "Checking exclusive database ownership.")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Serialize key maintenance with provider configuration and other operator
	// actions. The database lock protects SQLite state; this deployment lock
	// also prevents a concurrent service reconfiguration from racing the stop
	// and restart window below.
	releaseDeployment, err := ops.LockDeployment(ctx, ops.Deployment{Root: *root})
	if err != nil {
		return fmt.Errorf("acquire deployment operation lock: %w", err)
	}
	defer releaseDeployment()
	if err := checkDeploymentKeySources(*root); err != nil {
		return err
	}
	if err := checkComposeKeySources(ctx, *root); err != nil {
		return err
	}
	resume := func() error { return nil }
	if !*offline {
		var pauseErr error
		resume, pauseErr = pauseControl(ctx, *root)
		if pauseErr != nil {
			return pauseErr
		}
	}
	resumed := false
	defer func() {
		if !resumed {
			if err := resume(); err != nil {
				fmt.Fprintln(errOut, "Service recovery failed; restart the control service manually.")
			}
		}
	}()
	options := ops.KeyOptions{DatabasePath: *db, OutputPath: *output, ServerURL: *server}
	var result ops.KeyResult
	if rotate {
		result, err = ops.RotateKey(ctx, options)
	} else {
		result, err = ops.InitKey(ctx, options)
	}
	if err != nil {
		return err
	}
	if err := resume(); err != nil {
		return fmt.Errorf("key committed to private export %s; service restart failed: %w", *output, err)
	}
	resumed = true
	fmt.Fprintf(out, "Private key export: %s (0600)\n", *output)
	terminal, err := secretTerminal()
	if err != nil {
		fmt.Fprintln(out, "No controlling terminal; connection details are saved only in the private export.")
		return nil
	}
	defer terminal.Close()
	fmt.Fprintf(terminal, "Server URL: %s\nInstance key and app connection link (shown once):\n", *server)
	if err := result.Display(terminal); err != nil {
		return err
	}
	return renderQR(ctx, result.InstallationURL, terminal)
}

// pauseControl stops only a currently running managed control service and
// returns a recovery action. Lock acquisition in ops is still authoritative.
func pauseControl(ctx context.Context, root string) (func() error, error) {
	m := opsManager()
	d, err := ops.DiscoverDeployment(ctx, m, root)
	if err != nil {
		return nil, fmt.Errorf("discover control service (use --offline only after stopping it manually): %w", err)
	}
	recovery := func(name string, args ...string) func() error {
		return func() error {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := m.Run(recoveryCtx, name, args...)
			return err
		}
	}
	if d.HasSystemdRunner() {
		state, err := m.Run(ctx, "systemctl", "is-active", "zoe-compose.service")
		if strings.TrimSpace(string(state)) == "inactive" {
			return func() error { return nil }, nil
		}
		if err != nil || strings.TrimSpace(string(state)) != "active" {
			return nil, errors.New("cannot establish control service state; stop it manually and use --offline")
		}
		if _, err := m.Run(ctx, "systemctl", "stop", "zoe-compose.service"); err != nil {
			return nil, errors.New("cannot stop control service")
		}
		return recovery("systemctl", "start", "zoe-compose.service"), nil
	}
	if d.Mode != ops.ModeCompose || d.ComposeFile == "" {
		return nil, errors.New("unknown control deployment; stop it manually and use --offline")
	}
	base := []string{"compose", "--env-file", d.StackEnv, "--file", d.ComposeFile}
	if d.ComposeProject != "" {
		base = append(base, "--project-name", d.ComposeProject)
	}
	running, err := m.Run(ctx, "docker", append(append([]string{}, base...), "ps", "--status", "running", "--services")...)
	if err != nil {
		return nil, errors.New("cannot inspect control container")
	}
	active := false
	for _, service := range strings.Fields(string(running)) {
		if service == "zoe-server" {
			active = true
		}
	}
	if !active {
		return func() error { return nil }, nil
	}
	if _, err := m.Run(ctx, "docker", append(append([]string{}, base...), "stop", "zoe-server")...); err != nil {
		return nil, errors.New("cannot stop control container")
	}
	return recovery("docker", append(base, "start", "zoe-server")...), nil
}

// Startup credentials supplied by the deployment can overwrite or reject a
// rotated database key. Refuse mutation rather than leave a stale deployment.
func checkDeploymentKeySources(root string) error {
	for _, name := range []string{"ZOE_INSTANCE_KEY", "ZOE_INSTANCE_KEY_FILE"} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%s is set; remove the legacy startup credential source before key maintenance; the server uses its database credential", name)
		}
	}
	for _, file := range []string{"zoe-stack.env", "zoe-server.env", "zoe-runner.env", ".env"} {
		data, err := os.ReadFile(filepath.Join(root, file))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot inspect deployment credential sources: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
			name, value, ok := strings.Cut(line, "=")
			if ok && (strings.TrimSpace(name) == "ZOE_INSTANCE_KEY" || strings.TrimSpace(name) == "ZOE_INSTANCE_KEY_FILE") && strings.Trim(strings.TrimSpace(value), "\"'") != "" {
				return fmt.Errorf("%s configures a startup instance credential; remove or migrate that source before offline key maintenance to avoid stale credentials on restart", file)
			}
		}
	}
	return nil
}

// When a managed deployment selects a Compose file, inspect its resolved
// server environment before changing the database. A literal secret path can
// otherwise bypass the environment-file check above.
func checkComposeKeySources(ctx context.Context, root string) error {
	m := opsManager()
	d, err := ops.DiscoverDeployment(ctx, m, root)
	if err != nil {
		return errors.New("cannot inspect startup credential sources")
	}
	if d.ComposeFile == "" {
		return nil
	}
	args := []string{"compose", "--env-file", d.StackEnv, "--file", d.ComposeFile}
	if d.ComposeProject != "" {
		args = append(args, "--project-name", d.ComposeProject)
	}
	args = append(args, "config", "--format", "json")
	data, err := m.Run(ctx, "docker", args...)
	if err != nil {
		return errors.New("cannot resolve Compose startup credential sources; no key was changed")
	}
	var config struct {
		Services map[string]struct {
			Environment map[string]*string `json:"environment"`
		} `json:"services"`
	}
	if json.Unmarshal(data, &config) != nil {
		return errors.New("cannot parse Compose startup credential sources; no key was changed")
	}
	server, ok := config.Services["zoe-server"]
	if !ok {
		return errors.New("Compose control service is missing; no key was changed")
	}
	for _, name := range []string{"ZOE_INSTANCE_KEY", "ZOE_INSTANCE_KEY_FILE"} {
		if value := server.Environment[name]; value != nil && strings.TrimSpace(*value) != "" {
			return errors.New("Compose injects a legacy instance key; remove that startup source before key maintenance")
		}
	}
	if value := server.Environment["ZOE_SECRET_FILE_MAP"]; value != nil {
		for _, mapping := range strings.Split(*value, ",") {
			name, _, _ := strings.Cut(mapping, "=")
			if strings.TrimSpace(name) == "ZOE_INSTANCE_KEY" {
				return errors.New("Compose injects a legacy instance key through a secret file; remove that startup source before key maintenance")
			}
		}
	}
	return nil
}

// renderQR is self-contained; sensitive links never become process arguments.
func renderQR(_ context.Context, link string, out io.Writer) error {
	code, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return fmt.Errorf("encode installation QR: %w", err)
	}
	matrix := code.Bitmap()
	for y := 0; y < len(matrix); y += 2 {
		if _, err := io.WriteString(out, "\x1b[30;47m"); err != nil {
			return err
		}
		for x := range matrix[y] {
			top := matrix[y][x]
			bottom := y+1 < len(matrix) && matrix[y+1][x]
			glyph := " "
			switch {
			case top && bottom:
				glyph = "█"
			case top:
				glyph = "▀"
			case bottom:
				glyph = "▄"
			}
			if _, err := io.WriteString(out, glyph); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(out, "\x1b[0m\n"); err != nil {
			return err
		}
	}
	return nil
}

func statusOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(errOut)
	f := addProviderFlags(fs, false)
	optRoot := fs.String("opt-root", "/opt/zoe", "release installation root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	d, cfg, err := providerDeployment(ctx, f)
	if err != nil {
		return err
	}
	inspect := d.InspectConfig()
	inspect.SecretPath = cfg.SecretPath
	inspect.ProviderBaseURL = cfg.BaseURL
	inspect.ProviderModel = cfg.Model
	inspect.ProviderReasoningEffort = "unsupported"
	memory, memoryErr := d.MemoryConfig()
	if memoryErr != nil {
		return memoryErr
	}
	inspect.MemorySecretPath = memory.SecretPath
	inspect.MemoryBaseURL = memory.BaseURL
	inspect.MemoryModel = memory.Model
	inspect.MemoryReasoningEffort = memory.ReasoningEffort
	triage, triageErr := d.TriageConfig()
	if triageErr != nil {
		return triageErr
	}
	inspect.TriageBaseURL = triage.BaseURL
	inspect.TriageModel = triage.Model
	inspect.TriageSecretPath = triage.SecretPath
	status := opsManager().Inspect(ctx, inspect)
	summary, err := d.SummaryConfig()
	if err != nil {
		return err
	}
	answer, err := d.AnswerConfig()
	if err != nil {
		return err
	}
	for _, warning := range ops.ModelKeyWarnings(triage, ops.TriageConfig{BaseURL: memory.BaseURL, SecretPath: memory.SecretPath}, ops.TriageConfig{BaseURL: summary.BaseURL, SecretPath: summary.SecretPath}, ops.TriageConfig{BaseURL: answer.BaseURL, SecretPath: answer.SecretPath}) {
		fmt.Fprintln(out, warning)
	}
	fmt.Fprintf(out, "Deployment: %s\n", status.Mode)
	keyState := "pending (run zoe key init)"
	probeCtx, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	defer probeCancel()
	readyURL := fmt.Sprintf("http://127.0.0.1:%d/readyz", d.ServerPort())
	if req, requestErr := http.NewRequestWithContext(probeCtx, http.MethodGet, readyURL, nil); requestErr == nil {
		if response, requestErr := (&http.Client{Timeout: 2 * time.Second}).Do(req); requestErr == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				keyState = "configured"
			}
		}
	}
	fmt.Fprintf(out, "Instance key: %s\n", keyState)
	for _, service := range status.Services {
		fmt.Fprintf(out, "%s: %s (%s)\n", safeLabel(service.Name), safeLabel(service.State), safeLabel(service.Source))
	}
	fmt.Fprintf(out, "Provider: %s model=%s effort=%s source=%s last-check=%s\n", safeLabel(status.Provider.BaseURL), safeLabel(status.Provider.Model), safeLabel(status.Provider.ReasoningEffort), safeLabel(status.Provider.ConfigSource), safeLabel(ops.LastProviderCheck(d)))
	fmt.Fprintf(out, "Secret: %s exists=%t mode=%s owner=%s\n", safeLabel(status.Secret.Path), status.Secret.Exists, safeLabel(status.Secret.Mode), safeLabel(status.Secret.Owner))
	memoryHost := ""
	if u, parseErr := url.Parse(status.Memory.BaseURL); parseErr == nil {
		memoryHost = u.Hostname()
	}
	fmt.Fprintf(out, "Memory: host=%s model=%s effort=%s secret-file=%s mode-0600=%t last-check=%s\n", safeLabel(memoryHost), safeLabel(status.Memory.Model), safeLabel(status.Memory.ReasoningEffort), safeLabel(status.Memory.Secret.Path), status.Memory.Secret.Exists && status.Memory.Secret.Mode == "0600", safeLabel(status.Memory.LastCheck))
	triageHost := safeHost(inspect.TriageBaseURL)
	triagePresent := false
	if inspect.TriageSecretPath != "" {
		if info, statErr := os.Stat(inspect.TriageSecretPath); statErr == nil && info.Mode().IsRegular() {
			triagePresent = true
		}
	}
	triageState := "disabled"
	if inspect.TriageBaseURL != "" {
		triageState = "enabled"
	}
	style := triage.APIStyle
	if style == "" {
		style = "auto"
	}
	fmt.Fprintf(out, "Triage: %s style=%s host=%s model=%s effort=%s thinking=%s key-file-present=%t\n", triageState, safeLabel(style), safeLabel(triageHost), safeLabel(inspect.TriageModel), safeLabel(triage.ReasoningEffort), safeLabel(triage.Thinking), triagePresent)
	if err := printRecallStatus(d, out); err != nil {
		return err
	}
	current := filepath.Join(*optRoot, "releases", "current")
	if revision, readErr := os.ReadFile(filepath.Join(current, "SOURCE_REVISION")); readErr == nil {
		fmt.Fprintf(out, "Release: source-revision=%s\n", safeLabel(strings.TrimSpace(string(revision))))
	}
	if stack, readErr := os.ReadFile(filepath.Join(d.Root, "zoe-stack.env")); readErr == nil {
		for _, line := range strings.Split(string(stack), "\n") {
			if strings.HasPrefix(line, "ZOE_SERVER_IMAGE=") || strings.HasPrefix(line, "ZOE_MEMORY_WORKER_IMAGE=") {
				fmt.Fprintf(out, "Image: %s\n", safeLabel(line))
			}
		}
	}
	return nil
}

func safeLabel(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 80 {
		value = value[:80]
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "invalid"
		}
	}
	return value
}

// terminalEcho is replaced in tests; production uses the platform terminal control.
var terminalEcho = disableTerminalEcho

func readToken(in io.Reader, out io.Writer) ([]byte, error) {
	in = sharedInput(in)
	fmt.Fprint(out, "API token: ")
	if file := terminalFile(in); file != nil {
		restore, isTerminal, err := terminalEcho(file)
		if err != nil {
			return nil, errors.New("cannot hide terminal input; use --token-file")
		}
		if isTerminal {
			stop := make(chan struct{})
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
			go func() {
				select {
				case <-signals:
					restore()
					os.Exit(130)
				case <-stop:
				}
			}()
			defer func() {
				signal.Stop(signals)
				close(stop)
				restore()
				fmt.Fprintln(out)
			}()
		}
	}
	value, err := inputLine(in)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, errors.New("API token is required")
	}
	return []byte(value), nil
}

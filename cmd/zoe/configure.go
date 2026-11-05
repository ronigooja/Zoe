package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/ops"
)

// ConfigureMenuIO makes the interactive menu deterministic in tests and keeps
// secret input behind the same no-echo boundary used by the subcommands.
type ConfigureMenuIO struct {
	Root         string
	In           io.Reader
	Out          io.Writer
	ErrOut       io.Writer
	PromptSecret func(io.Writer) ([]byte, error)
}

func configureMenuIO(root string, in io.Reader, out, errOut io.Writer) ConfigureMenuIO {
	return ConfigureMenuIO{Root: root, In: in, Out: out, ErrOut: errOut, PromptSecret: func(w io.Writer) ([]byte, error) {
		return readToken(in, w)
	}}
}

func menuModel(m ConfigureMenuIO, triage bool) error {
	root := m.Root
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := ops.DiscoverDeployment(ctx, opsManager(), root)
	if err != nil {
		return err
	}
	var base, model, keyPath string
	if triage {
		c, e := d.TriageConfig()
		if e != nil {
			return e
		}
		base, model, keyPath = c.BaseURL, c.Model, c.SecretPath
		if keyPath == "" {
			keyPath = filepath.Join(root, "secrets", "triage-api-key")
		}
	} else {
		c, e := d.MemoryConfig()
		if e != nil {
			return e
		}
		base, model, keyPath = c.BaseURL, c.Model, c.SecretPath
	}
	kind := "Memory"
	if triage {
		kind = "Triage"
	}
	fmt.Fprintf(m.Out, "%s model configuration (Enter keeps current value)\n", kind)
	base, err = menuValue(m, "Base URL", base)
	if err != nil {
		return err
	}
	model, err = menuValue(m, "Model", model)
	if err != nil {
		return err
	}
	keyChoice, err := menuValue(m, "Key file (Enter uses current)", keyPath)
	if err != nil {
		return err
	}
	if keyChoice != "" {
		keyPath = keyChoice
	}
	if triage {
		return menuSaveTriage(m, d, base, model, keyPath)
	}
	return menuSaveMemory(m, d, base, model, keyPath)
}

func menuValue(m ConfigureMenuIO, label, current string) (string, error) {
	if current != "" {
		fmt.Fprintf(m.Out, "%s [%s]: ", label, safeLabel(current))
	} else {
		fmt.Fprintf(m.Out, "%s: ", label)
	}
	v, err := inputLine(sharedInput(m.In))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(v) == "" {
		return current, nil
	}
	return strings.TrimSpace(v), nil
}

func menuSaveTriage(m ConfigureMenuIO, d ops.Deployment, base, model, keyPath string) error {
	if err := ops.ValidateModelBaseURL(base); err != nil {
		return err
	}
	if strings.TrimSpace(model) == "" {
		return errors.New("triage model is required")
	}
	fmt.Fprint(m.Out, "API key (input is hidden; leave empty to keep current): ")
	secret, err := m.PromptSecret(m.Out)
	if err != nil {
		return err
	}
	if len(secret) > 0 {
		if err := ops.WriteTriageSecretAtomic(keyPath, secret); err != nil {
			return err
		}
		for i := range secret {
			secret[i] = 0
		}
	} else if err := ops.ValidateKeyFile(keyPath); err != nil {
		return err
	}
	c := ops.TriageConfig{BaseURL: base, Model: model, SecretPath: keyPath}
	if err := ops.WriteTriageEnvAtomic(d.StackEnv, c, false); err != nil {
		return err
	}
	fmt.Fprintln(m.Out, "Triage model configured.")
	fmt.Fprint(m.Out, "Test now? [Y/n]: ")
	answer, err := inputLine(sharedInput(m.In))
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) == "" || strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, testErr := ops.TestTriage(ctx, c, nil)
		if testErr != nil {
			fmt.Fprintf(m.ErrOut, "FAIL HTTP %dxx latency=%dms: %s\n", r.HTTPStatus/100, r.Latency.Milliseconds(), safeLabel(testErr.Error()))
		} else {
			fmt.Fprintf(m.Out, "PASS HTTP %dxx latency=%dms category=%s\n", r.HTTPStatus/100, r.Latency.Milliseconds(), safeLabel(r.Category))
		}
	}
	fmt.Fprintln(m.Out, "Apply with: zoe update")
	return nil
}

func menuSaveMemory(m ConfigureMenuIO, d ops.Deployment, base, model, keyPath string) error {
	if err := ops.ValidateModelBaseURL(base); err != nil {
		return err
	}
	if strings.TrimSpace(model) == "" {
		return errors.New("memory model is required")
	}
	provider, err := d.ProviderConfig()
	if err != nil {
		return err
	}
	fmt.Fprint(m.Out, "API key (input is hidden; leave empty to keep current): ")
	secret, err := m.PromptSecret(m.Out)
	if err != nil {
		return err
	}
	cfg := ops.MemoryConfig{BaseURL: base, Model: model, SecretPath: keyPath}
	if len(secret) > 0 {
		cfg.Secret = strings.TrimSpace(string(secret))
		for i := range secret {
			secret[i] = 0
		}
	} else {
		if err := ops.ValidateKeyFile(keyPath); err != nil {
			return err
		}
		key, readErr := os.ReadFile(keyPath)
		if readErr != nil {
			return errors.New("key file is unavailable")
		}
		cfg.Secret = strings.TrimSpace(string(key))
		for i := range key {
			key[i] = 0
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := opsManager().ConfigureMemory(ctx, d, cfg, provider.SecretPath, nil)
	fmt.Fprintf(m.Out, "HTTP status: %d\nModel available: %t\n", result.HTTPStatus, result.ModelAvailable)
	if err != nil {
		return err
	}
	fmt.Fprint(m.Out, "Test now? [Y/n]: ")
	answer, err := inputLine(sharedInput(m.In))
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) == "" || strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		testResult, testErr := opsManager().TestMemory(ctx, d, cfg, nil)
		if testErr != nil {
			fmt.Fprintf(m.ErrOut, "FAIL HTTP %dxx\n", testResult.HTTPStatus/100)
		} else {
			fmt.Fprintf(m.Out, "PASS HTTP %dxx\n", testResult.HTTPStatus/100)
		}
	}
	fmt.Fprintln(m.Out, "Apply with: zoe update")
	return nil
}

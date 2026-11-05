package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/ops"
)

func init() {
	for _, kind := range []string{"summary", "answer"} {
		_ = RegisterOperation(kind+" configure", func(args []string, in io.Reader, out, errOut io.Writer) error {
			return recallConfigureOperation(kind, args, in, out, errOut)
		})
		_ = RegisterOperation(kind+" test", func(args []string, in io.Reader, out, errOut io.Writer) error {
			return recallTestOperation(kind, args, in, out, errOut)
		})
	}
}

func recallConfigureOperation(kind string, args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet(kind+" configure", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	base := fs.String("base-url", "", "model base URL")
	model := fs.String("model", "", "model")
	effort := fs.String("effort", "", "reasoning effort (low, medium, high, or empty)")
	thinking := fs.String("thinking", "", "thinking (enabled, disabled, or empty)")
	keyStdin := fs.Bool("key-stdin", false, "read private API key from stdin")
	disable := fs.Bool("disable", false, "disable without deleting other settings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	d, err := ops.DiscoverDeployment(context.Background(), opsManager(), *root)
	if err != nil {
		return err
	}
	c, err := d.RecallModelConfig(kind)
	if err != nil {
		return err
	}
	if hasFlag(args, "--base-url") {
		c.BaseURL = *base
	}
	if hasFlag(args, "--model") {
		c.Model = *model
	}
	if hasFlag(args, "--effort") {
		c.ReasoningEffort = *effort
	}
	if hasFlag(args, "--thinking") {
		c.Thinking = *thinking
	}
	if *disable {
		if err := ops.WriteRecallModelEnvAtomic(d.StackEnv, kind, c, true); err != nil {
			return err
		}
		fmt.Fprintln(out, "Saved.")
		fmt.Fprintln(out, "Apply with: zoe update")
		return nil
	}
	c, err = c.Normalized(kind)
	if err != nil {
		return err
	}
	var secret []byte
	if *keyStdin {
		secret, err = io.ReadAll(io.LimitReader(in, 65537))
		if err != nil {
			return errors.New("cannot read key from stdin")
		}
		if len(secret) > 65536 {
			return errors.New("key is too large")
		}
	}
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	if err := saveRecallModel(d, kind, c, secret); err != nil {
		return err
	}
	fmt.Fprintln(out, "Saved.")
	fmt.Fprintln(out, "Apply with: zoe update")
	return nil
}

func saveRecallModel(d ops.Deployment, kind string, c ops.RecallModelConfig, secret []byte) error {
	return ops.SaveRecallModel(context.Background(), d, kind, c, secret)
}

func recallTestOperation(kind string, args []string, _ io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet(kind+" test", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	d, err := ops.DiscoverDeployment(context.Background(), opsManager(), *root)
	if err != nil {
		return err
	}
	c, err := d.RecallModelConfig(kind)
	if err != nil {
		return err
	}
	return printRecallProbe(kind, c, out, errOut)
}

func printRecallProbe(kind string, c ops.RecallModelConfig, out, errOut io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r, err := ops.TestRecallModel(ctx, kind, c, nil)
	class := "PASS"
	if err != nil {
		class = "FAIL"
	}
	fmt.Fprintf(out, "%s style=%s HTTP %dxx latency=%dms\n", class, safeLabel(r.Style), r.HTTPStatus/100, r.Latency.Milliseconds())
	if err != nil {
		fmt.Fprintln(errOut, "Model probe failed; check endpoint configuration and credentials.")
	}
	return err
}

func menuRecallModel(m ConfigureMenuIO, kind string) error {
	d, err := ops.DiscoverDeployment(context.Background(), opsManager(), m.Root)
	if err != nil {
		return err
	}
	c, err := d.RecallModelConfig(kind)
	if err != nil {
		return err
	}
	fmt.Fprintf(m.Out, "%s model configuration (Enter keeps current value)\n", strings.ToUpper(kind[:1])+kind[1:])
	for _, field := range []struct {
		label    string
		value    *string
		validate func(string) error
	}{
		{"Base URL", &c.BaseURL, ops.ValidateModelBaseURL},
		{"Model", &c.Model, func(v string) error {
			if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n") {
				return errors.New("model is required and must not contain newlines")
			}
			return nil
		}},
		{"Reasoning effort (low/medium/high; empty keeps default)", &c.ReasoningEffort, ops.ValidateReasoningEffort},
		{"Thinking (Enter = provider default, disabled = faster, enabled)", &c.Thinking, func(v string) error {
			if v != "" && v != "enabled" && v != "disabled" {
				return errors.New("thinking must be enabled, disabled, or empty")
			}
			return nil
		}},
		{"Key file (Enter uses current)", &c.SecretPath, func(v string) error {
			if !filepath.IsAbs(v) {
				return errors.New("key file must be absolute")
			}
			return nil
		}},
	} {
		for {
			*field.value, err = menuValue(m, field.label, *field.value)
			if err != nil {
				return err
			}
			if err = field.validate(*field.value); err != nil {
				fmt.Fprintf(m.ErrOut, "%s Nothing was saved.\n", err)
				continue
			}
			break
		}
	}
	// Recall credentials default to independent destinations under the deployment root.
	if _, err = c.Normalized(kind); err != nil {
		fmt.Fprintln(m.ErrOut, "Nothing was saved.")
		return err
	}
	fmt.Fprint(m.Out, "API key (input is hidden; leave empty to keep current): ")
	secret, err := m.PromptSecret(m.Out)
	if err != nil {
		fmt.Fprintln(m.Out, "Cancelled. Nothing was saved.")
		return err
	}
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	if err = saveRecallModel(d, kind, c, secret); err != nil {
		fmt.Fprintf(m.ErrOut, "%s Nothing was saved.\n", err)
		return err
	}
	fmt.Fprintln(m.Out, "Saved.")
	fmt.Fprint(m.Out, "Test now? [Y/n]: ")
	choice, err := inputLine(sharedInput(m.In))
	if err != nil {
		return err
	}
	if strings.TrimSpace(choice) == "" || strings.EqualFold(strings.TrimSpace(choice), "y") || strings.EqualFold(strings.TrimSpace(choice), "yes") {
		_ = printRecallProbe(kind, c, m.Out, m.ErrOut)
	}
	fmt.Fprintln(m.Out, "Apply with: zoe update")
	return nil
}

func printRecallStatus(d ops.Deployment, out io.Writer) error {
	for _, kind := range []string{"summary", "answer"} {
		c, err := d.RecallModelConfig(kind)
		if err != nil {
			return err
		}
		state := "disabled"
		if c.BaseURL != "" {
			state = "enabled"
		}
		present := false
		if info, err := os.Stat(c.SecretPath); err == nil && info.Mode().IsRegular() {
			present = true
		}
		timeout := c.TimeoutMS
		if timeout == 0 {
			if kind == "summary" {
				timeout = 120000
			} else {
				timeout = 8000
			}
		}
		fmt.Fprintf(out, "%s: %s style=auto host=%s model=%s effort=%s thinking=%s timeout-ms=%d key-file-present=%t\n", strings.ToUpper(kind[:1])+kind[1:], state, safeHost(c.BaseURL), safeLabel(c.Model), safeLabel(c.ReasoningEffort), safeLabel(c.Thinking), timeout, present)
	}
	return nil
}

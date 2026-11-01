package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/ops"
)

func init() {
	_ = RegisterOperation("voice configure", voiceConfigureOperation)
	_ = RegisterOperation("voice test", voiceTestOperation)
	_ = RegisterOperation("voice status", voiceStatusOperation)
}

func voiceConfigureOperation(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("voice configure", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
	base := fs.String("url", "", "remote chatgpt-web-voice downstream HTTPS URL")
	token := fs.String("token", "", "compatibility API key (prefer --token-file or prompt)")
	tokenFile := fs.String("token-file", "", "read compatibility API key from a private file")
	secretFile := fs.String("secret-file", "", "private destination for the API key")
	nonInteractive := fs.Bool("non-interactive", false, "require URL and token without prompting")
	enableMedia := fs.Bool("enable-account-media", false, "enable experimental account lease integration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := ops.DiscoverDeployment(ctx, opsManager(), *root)
	if err != nil {
		return err
	}
	if d.ComposeFile == "" {
		return errors.New("voice configure requires a known Compose file; configure ZOE_COMPOSE_FILE in the stack environment")
	}
	in = sharedInput(in)
	if strings.TrimSpace(*base) == "" {
		if *nonInteractive {
			return errors.New("--url is required")
		}
		fmt.Fprint(out, "Voice gateway HTTPS URL: ")
		*base, err = inputLine(in)
		if err != nil {
			return err
		}
	}
	if err = validateVoiceURL(*base); err != nil {
		return err
	}
	if *token != "" && *tokenFile != "" {
		return errors.New("use only one of --token and --token-file")
	}
	secret := []byte(*token)
	if *tokenFile != "" {
		secret, err = os.ReadFile(*tokenFile)
		if err != nil {
			return errors.New("cannot read voice API key file")
		}
	}
	if len(secret) == 0 {
		if *nonInteractive {
			return errors.New("--token-file or --token is required")
		}
		secret, err = readToken(in, out)
		if err != nil {
			return err
		}
	}
	value := strings.TrimSpace(string(secret))
	for i := range secret {
		secret[i] = 0
	}
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return errors.New("voice API key is empty or invalid")
	}
	if *secretFile == "" {
		*secretFile = filepath.Join(*root, "secrets", "voice-gateway-api-key")
	}
	if !filepath.IsAbs(*secretFile) {
		return errors.New("--secret-file must be absolute")
	}
	if err = ops.WritePrivateFile(*secretFile, []byte(value+"\n")); err != nil {
		return err
	}
	if err = ops.WriteVoiceEnvAtomic(d.StackEnv, *base, *secretFile, *enableMedia); err != nil {
		return err
	}
	fmt.Fprintf(out, "Voice gateway configuration saved. URL: %s\nAPI key file: %s\nAccount media: %t\n", *base, *secretFile, *enableMedia)
	// The Compose service is zoe-server; zoe-runner.service is a systemd
	// unit and is not a valid Compose target. Systemd supervises this Compose
	// stack, so the same reload is required in both deployment modes.
	if err = opsManager().ReloadCompose(ctx, []string{"zoe-server"}, d.ComposeProject, d.ComposeFile, d.StackEnv); err != nil {
		return fmt.Errorf("voice configuration saved but service reload failed: %w", err)
	}
	fmt.Fprintln(out, "Zoe services reloaded.")
	return nil
}

func voiceTestOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("voice test", flag.ContinueOnError)
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
	v, err := ops.ReadEnvForVoice(d.StackEnv)
	if err != nil {
		return err
	}
	key, err := os.ReadFile(v["ZOE_VOICE_GATEWAY_API_KEY_FILE"])
	if err != nil {
		return errors.New("cannot read configured voice API key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(v["ZOE_VOICE_GATEWAY_URL"], "/")+"/v1/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("voice gateway health returned HTTP %d", resp.StatusCode)
	}
	fmt.Fprintf(out, "Voice gateway: healthy (%s)\n", v["ZOE_VOICE_GATEWAY_URL"])
	return nil
}

func voiceStatusOperation(args []string, _ io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("voice status", flag.ContinueOnError)
	fs.SetOutput(errOut)
	root := fs.String("root", "/etc/zoe", "deployment configuration root")
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
	v, err := ops.ReadEnvForVoice(d.StackEnv)
	if err != nil {
		return err
	}
	path := v["ZOE_VOICE_GATEWAY_API_KEY_FILE"]
	info, statErr := os.Stat(path)
	fmt.Fprintf(out, "Voice gateway URL: %s\nAPI key configured: %t\nAPI key file: %s\nAccount media: %s\n", v["ZOE_VOICE_GATEWAY_URL"], statErr == nil && info.Mode().Perm()&0077 == 0, path, v["ZOE_VOICE_ACCOUNT_MEDIA_ENABLED"])
	return nil
}

func validateVoiceURL(value string) error {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("voice gateway URL must be an HTTPS URL without query or credentials")
	}
	return nil
}

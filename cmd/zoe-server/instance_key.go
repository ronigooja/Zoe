package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Key material is exported only at an explicit operator-selected destination.
func manageInstanceKey(command string, args []string) error {
	if command != "init-key" && command != "rotate-key" {
		return errors.New("usage: zoe-server init-key|rotate-key --database PATH (--output PATH | --stdout) [--server URL]")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	dbPath := flags.String("database", "", "control database path (stop the server first)")
	output := flags.String("output", "", "new private key export file; must not already exist")
	stdout := flags.Bool("stdout", false, "explicitly export key to standard output")
	server := flags.String("server", "", "public server URL to include in Apple installation link")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" || flags.NArg() != 0 || (*output == "") == (!*stdout) {
		return errors.New("specify --database and exactly one of --output or --stdout")
	}
	if *server != "" {
		if err := validateInstallationServer(*server); err != nil {
			return err
		}
	}

	var file *os.File
	var err error
	if *output != "" {
		file, err = os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
	}
	committed := false
	defer func() {
		if !committed && file != nil {
			_ = os.Remove(*output)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, storage.Config{Path: *dbPath})
	if err != nil {
		return err
	}
	defer store.Close()
	if err = storage.RegisterCoreMigrations(store); err != nil {
		return err
	}
	// Register the complete migration set before opening an existing instance.
	if err = registerKeyCommandMigrations(store); err != nil {
		return err
	}
	if err = store.Migrate(ctx); err != nil {
		return err
	}
	key, err := control.GenerateInstanceKey()
	if err != nil {
		return err
	}
	export := struct {
		Key             string `json:"key"`
		InstallationURL string `json:"installation_url,omitempty"`
	}{Key: key}
	if *server != "" {
		export.InstallationURL = "zoe://connect?" + url.Values{"server": {*server}}.Encode() + "#" + url.Values{"key": {key}}.Encode()
	}
	// Persist a private file before committing the hash, so disk failures cannot
	// leave an initialized instance whose generated key was never saved.
	if file != nil {
		if err = json.NewEncoder(file).Encode(export); err != nil {
			return err
		}
		if err = file.Sync(); err != nil {
			return err
		}
	}
	if command == "init-key" {
		var count int
		if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM instance_auth`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("instance credential already exists; use rotate-key")
		}
		_, _, err = control.InitializeInstanceAuth(ctx, store, key)
	} else {
		err = control.RotateInstanceKey(ctx, store, key)
	}
	if err != nil {
		return err
	}
	committed = true
	if file == nil {
		if err = json.NewEncoder(os.Stdout).Encode(export); err != nil {
			return fmt.Errorf("key committed but stdout export failed; rotate again with --output: %w", err)
		}
	}
	return nil
}

func registerKeyCommandMigrations(store *storage.Store) error {
	if err := control.RegisterInstanceMaintenanceMigration(store); err != nil {
		return err
	}
	if err := memoryapi.RegisterMigrations(store); err != nil {
		return err
	}
	if err := voice.RegisterMigrations(store); err != nil {
		return err
	}
	return capabilities.RegisterMigrations(store)
}

func validateInstallationServer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || strings.ContainsAny(raw, "?#\\") || strings.HasSuffix(u.Host, ":") {
		return errors.New("server must be a valid http(s) URL without credentials, query, or fragment")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return errors.New("server port must be between 1 and 65535")
		}
	}
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return errors.New("server path cannot contain encoded separators")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("server path cannot contain dot segments")
		}
	}
	return nil
}

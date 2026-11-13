// zoe-maintenance performs offline authoritative instance-fence operations.
// It must run under the service account and obtains storage.Store's exclusive
// lock on the database before changing any authoritative state.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/memoryapi"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "zoe-maintenance: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: zoe-maintenance status|fence|deletion-head|prepare-restore|reconcile-deletions|enable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch args[0] {
	case "status":
		flags := flag.NewFlagSet("status", flag.ContinueOnError)
		database := flags.String("database", "", "authoritative database path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		store, err := openStore(ctx, *database)
		if err != nil {
			return err
		}
		defer store.Close()
		state, err := controlState(ctx, store)
		if err != nil {
			return err
		}
		tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		readiness, err := control.MigrationReadinessTx(ctx, tx)
		_ = tx.Rollback()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			control.InstanceState
			control.MigrationReadiness
			MigrationReady bool `json:"migration_ready"`
		}{state, readiness, !state.DispatchEnabled && readiness.Ready()})
	case "fence":
		flags := flag.NewFlagSet("fence", flag.ContinueOnError)
		database := flags.String("database", "", "authoritative database path")
		reason := flags.String("reason", "", "fence reason")
		expected := flags.Int64("expected-epoch", 0, "expected current instance epoch")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		store, err := openStore(ctx, *database)
		if err != nil {
			return err
		}
		defer store.Close()
		state, err := control.FenceInstance(ctx, store, *reason, *expected)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(state)
	case "deletion-head":
		flags := flag.NewFlagSet("deletion-head", flag.ContinueOnError)
		database := flags.String("database", "", "current authoritative database path")
		output := flags.String("output", "", "new protected deletion-head JSON path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *output == "" {
			return errors.New("deletion-head requires --output")
		}
		store, err := openStore(ctx, *database)
		if err != nil {
			return err
		}
		defer store.Close()
		var lineage string
		var generation, sequence int64
		if err = store.DB().QueryRowContext(ctx, `SELECT lineage_id,generation,deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&lineage, &generation, &sequence); err != nil {
			return err
		}
		journal, err := storage.NewDeletionJournal(store.Path())
		if err != nil {
			return err
		}
		records, err := journal.ReadAll()
		if err != nil {
			return err
		}
		if int64(len(records)) != sequence {
			return errors.New("authority deletion sequence differs from current journal")
		}
		headHash := ""
		recordGeneration := int64(0)
		if len(records) > 0 {
			last := records[len(records)-1]
			if last.LineageID != lineage || last.Generation > generation {
				return errors.New("current deletion journal does not match authority identity")
			}
			headHash = last.Hash
			recordGeneration = last.Generation
		}
		journalBytes, err := os.ReadFile(store.Path() + ".deletion-ledger")
		if errors.Is(err, os.ErrNotExist) {
			journalBytes = nil
		} else if err != nil {
			return err
		}
		fileHash := sha256.Sum256(journalBytes)
		attestation := map[string]any{"lineage_id": lineage, "generation": generation, "record_generation": recordGeneration, "sequence": sequence, "head_hash": headHash, "file_sha256": hex.EncodeToString(fileHash[:])}
		if err = os.MkdirAll(filepath.Dir(*output), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		encodeErr := json.NewEncoder(out).Encode(attestation)
		syncErr := out.Sync()
		closeErr := out.Close()
		if encodeErr != nil {
			return encodeErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	case "reconcile-deletions":
		flags := flag.NewFlagSet("reconcile-deletions", flag.ContinueOnError)
		database := flags.String("database", "", "restored authoritative database path")
		restoreStatePath := flags.String("restore-state", "", "private restore-state file written by zoe-restore.py")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		trusted, err := readTrustedDeletionHead(*restoreStatePath)
		if err != nil {
			return err
		}
		store, err := openStore(ctx, *database)
		if err != nil {
			return err
		}
		defer store.Close()
		result, err := storage.ReconcileDeletionJournalWithTrustedHead(ctx, store, trusted)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "prepare-restore":
		flags := flag.NewFlagSet("prepare-restore", flag.ContinueOnError)
		database := flags.String("database", "", "restored database path ending in .restore-pending")
		activateAs := flags.String("activate-as", "", "new final database path; must not exist")
		newID := flags.String("new-instance-id", "", "new target instance ID")
		expected := flags.Int64("expected-epoch", 0, "restored source epoch")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if !strings.HasSuffix(*database, ".restore-pending") || *activateAs == "" || *database == *activateAs {
			return errors.New("prepare-restore requires a .restore-pending database and a distinct --activate-as path")
		}
		if _, err := os.Lstat(*activateAs); err == nil {
			return errors.New("activation target already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		id := *newID
		if id == "" {
			var err error
			id, err = newInstanceID()
			if err != nil {
				return err
			}
		}
		store, err := openStore(ctx, *database)
		if err != nil {
			return err
		}
		state, err := control.PrepareRestoredInstance(ctx, store, id, *expected)
		if err == nil {
			_, err = store.DB().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		}
		closeErr := store.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = moveDatabaseFamily(*database, *activateAs); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(state)
	case "enable":
		flags := flag.NewFlagSet("enable", flag.ContinueOnError)
		database := flags.String("database", "", "target authoritative database path")
		expected := flags.Int64("expected-epoch", 0, "expected target epoch")
		proofFile := flags.String("proof-file", "", "signed migration isolation proof")
		keyFile := flags.String("proof-key-file", "", "deployment-managed 32-byte proof key, mode 0600")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		proofBytes, err := readPrivateFile(*proofFile, 1<<20)
		if err != nil {
			return err
		}
		key, err := readPrivateFile(*keyFile, 32)
		if err != nil || len(key) != 32 {
			return errors.New("proof key file must be a current-user-owned regular file, mode 0600, containing 32 bytes")
		}
		proof, err := control.VerifyIsolationProof(proofBytes, key)
		if err != nil {
			return err
		}
		store, err := openStore(ctx, *database)
		if err != nil {
			return err
		}
		defer store.Close()
		if err = control.RecordIsolationProof(ctx, store, proof); err != nil {
			return err
		}
		state, err := control.EnableInstance(ctx, store, *expected, proof)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(state)
	default:
		return fmt.Errorf("unknown operation %q", args[0])
	}
}

func readTrustedDeletionHead(path string) (storage.TrustedDeletionHead, error) {
	var trusted storage.TrustedDeletionHead
	if path == "" {
		return trusted, errors.New("--restore-state is required for deletion reconciliation")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return trusted, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return trusted, errors.New("restore-state must be a regular file with mode 0600")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return trusted, errors.New("restore-state must be owned by the maintenance user")
	}
	var envelope struct {
		RestoreState struct {
			TrustedDeletionHead *storage.TrustedDeletionHead `json:"trusted_deletion_head"`
		} `json:"restore_state"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return trusted, err
	}
	if err = json.Unmarshal(data, &envelope); err != nil {
		return trusted, fmt.Errorf("parse protected restore-state: %w", err)
	}
	if envelope.RestoreState.TrustedDeletionHead == nil {
		return trusted, errors.New("restore-state has no authenticated deletion head")
	}
	return *envelope.RestoreState.TrustedDeletionHead, nil
}

func openStore(ctx context.Context, database string) (*storage.Store, error) {
	if strings.TrimSpace(database) == "" {
		return nil, errors.New("--database is required")
	}
	store, err := storage.Open(ctx, storage.Config{Path: database})
	if err != nil {
		return nil, err
	}
	register := func(err error) (*storage.Store, error) {
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		return store, nil
	}
	if err = storage.RegisterCoreMigrations(store); err != nil {
		return register(err)
	}
	if err = memoryapi.RegisterMigrations(store); err != nil {
		return register(err)
	}
	if err = voice.RegisterMigrations(store); err != nil {
		return register(err)
	}
	if err = capabilities.RegisterMigrations(store); err != nil {
		return register(err)
	}
	if err = control.RegisterInstanceMaintenanceMigration(store); err != nil {
		return register(err)
	}
	if err = store.Migrate(ctx); err != nil {
		return register(err)
	}
	return store, nil
}

func controlState(ctx context.Context, store *storage.Store) (control.InstanceState, error) {
	tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return control.InstanceState{}, err
	}
	defer tx.Rollback()
	return control.GetInstanceState(ctx, tx)
}

func newInstanceID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func readPrivateFile(name string, max int64) ([]byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("private file path is required")
	}
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > max {
		return nil, errors.New("private file must be a regular file with mode 0600 and within size limit")
	}
	return os.ReadFile(name)
}

func moveDatabaseFamily(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm", ".deletion-ledger"} {
		from, to := source+suffix, destination+suffix
		if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("activation target already exists: %s", to)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
		if err := os.Chmod(to, 0o600); err != nil {
			return err
		}
	}
	return nil
}

package control

import (
	"context"
	"database/sql"
	"errors"
	"github.com/ronigooja/Zoe/contracts"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ronigooja/Zoe/internal/storage"
)

type exportTempLease struct {
	root *os.Root
	id   string
	file *os.File
	lock *exportTempLock
}

type exportTempLock struct {
	mu   sync.Mutex
	refs int
}

var exportTempLocks = struct {
	sync.Mutex
	items map[string]*exportTempLock
}{items: make(map[string]*exportTempLock)}

var inMemoryExportRoots sync.Map // map[*storage.Store]string; :memory: stores are test-only

func acquireExportTempLock(id string, wait bool) (*exportTempLock, bool) {
	exportTempLocks.Lock()
	entry := exportTempLocks.items[id]
	if entry == nil {
		entry = &exportTempLock{}
		exportTempLocks.items[id] = entry
	}
	entry.refs++
	exportTempLocks.Unlock()
	if wait {
		entry.mu.Lock()
		return entry, true
	}
	if entry.mu.TryLock() {
		return entry, true
	}
	exportTempLocks.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(exportTempLocks.items, id)
	}
	exportTempLocks.Unlock()
	return nil, false
}

func releaseExportTempLock(id string, entry *exportTempLock) {
	entry.mu.Unlock()
	exportTempLocks.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(exportTempLocks.items, id)
	}
	exportTempLocks.Unlock()
}

func exportTempRootPath(store *storage.Store) (string, error) {
	if store == nil {
		return "", errors.New("export registry store is required")
	}
	if store.Path() == ":memory:" {
		if existing, ok := inMemoryExportRoots.Load(store); ok {
			return existing.(string), nil
		}
		path, err := os.MkdirTemp("", "zoe-export-instance-")
		if err != nil {
			return "", err
		}
		if err = os.Chmod(path, 0700); err != nil {
			_ = os.RemoveAll(path)
			return "", err
		}
		actual, loaded := inMemoryExportRoots.LoadOrStore(store, path)
		if loaded {
			_ = os.RemoveAll(path)
			path = actual.(string)
		}
		return path, nil
	}
	databasePath, err := filepath.Abs(store.Path())
	if err != nil {
		return "", err
	}
	return databasePath + ".exports", nil
}

func openPrivateExportRoot(store *storage.Store, create bool) (*os.Root, error) {
	path, err := exportTempRootPath(store)
	if err != nil {
		return nil, err
	}
	if create {
		if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || before.Mode().Perm() != 0700 {
		return nil, errors.New("export temporary root is not a private directory")
	}
	if stat, ok := before.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("export temporary root owner does not match control server")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !os.SameFile(before, after) {
		_ = root.Close()
		return nil, errors.New("export temporary root changed while opening")
	}
	return root, nil
}

func (s *Server) beginExportTemp(ctx context.Context, p contracts.Principal, scope, exportID string) (*exportTempLease, error) {
	lock, ok := acquireExportTempLock(exportID, true)
	if !ok {
		return nil, errors.New("export lock unavailable")
	}
	root, err := openPrivateExportRoot(s.store, true)
	if err != nil {
		releaseExportTempLock(exportID, lock)
		return nil, err
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		_ = root.Close()
		releaseExportTempLock(exportID, lock)
		return nil, err
	}
	defer tx.Rollback()
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT deletion_sequence FROM zoe_instance_state WHERE singleton=1`).Scan(&sequence); err != nil {
		_ = root.Close()
		releaseExportTempLock(exportID, lock)
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO zoe_export_temp_registry(export_id,workspace_id,owner_id,export_scope,created_deletion_sequence,status,invalidation_sequence,created_at,updated_at,last_error) VALUES(?,?,?,?,?,'active',0,?,?, '')`, exportID, p.WorkspaceID, p.SubjectID, scope, sequence, now, now); err != nil {
		_ = root.Close()
		releaseExportTempLock(exportID, lock)
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		_ = root.Close()
		releaseExportTempLock(exportID, lock)
		return nil, err
	}
	file, err := root.OpenFile(exportDataName(exportID), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = root.Close()
		releaseExportTempLock(exportID, lock)
		return nil, err
	}
	return &exportTempLease{root: root, id: exportID, file: file, lock: lock}, nil
}

func exportDataName(id string) string    { return id + ".data.json" }
func exportArchiveName(id string) string { return id + ".archive.zip" }

func validExportID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func (l *exportTempLease) createArchive() (*os.File, error) {
	return l.root.OpenFile(exportArchiveName(l.id), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
}

func (l *exportTempLease) invalidated(ctx context.Context, db *sql.DB) (bool, error) {
	var status string
	err := db.QueryRowContext(ctx, `SELECT status FROM zoe_export_temp_registry WHERE export_id=?`, l.id).Scan(&status)
	if err != nil {
		return false, err
	}
	return status != "active", nil
}

func (l *exportTempLease) closeAndClean(db *sql.DB) {
	if l.file != nil {
		_ = l.file.Close()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := removeRegisteredExportFiles(l.root, l.id); err != nil {
		_, _ = db.Exec(`UPDATE zoe_export_temp_registry SET last_error='self_cleanup_failed',updated_at=? WHERE export_id=?`, now, l.id)
	} else {
		_, _ = db.Exec(`UPDATE zoe_export_temp_registry SET status='cleaned',updated_at=?,last_error='' WHERE export_id=?`, now, l.id)
	}
	_ = l.root.Close()
	releaseExportTempLock(l.id, l.lock)
}

func (s *Server) cleanExportTemps(ctx context.Context, workspace string, sequence int64) (bool, string, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.store.DB().ExecContext(ctx, `UPDATE zoe_export_temp_registry SET status='invalidated',invalidation_sequence=?,updated_at=?,last_error=''
WHERE workspace_id=? AND status='active' AND created_deletion_sequence<?`, sequence, now, workspace, sequence); err != nil {
		return false, "registry_unavailable", err
	}
	root, err := openPrivateExportRoot(s.store, true)
	if err != nil {
		return false, "unsafe_private_root", err
	}
	defer root.Close()
	rows, err := s.store.DB().QueryContext(ctx, `SELECT export_id,status FROM zoe_export_temp_registry ORDER BY export_id`)
	if err != nil {
		return false, "registry_unavailable", err
	}
	known := make(map[string]struct{})
	targets := make([]string, 0)
	for rows.Next() {
		var id, status string
		if err = rows.Scan(&id, &status); err != nil {
			_ = rows.Close()
			return false, "registry_unavailable", err
		}
		if !validExportID(id) {
			_ = rows.Close()
			return false, "adapter_missing", errors.New("export registry contains an invalid identifier")
		}
		known[exportDataName(id)] = struct{}{}
		known[exportArchiveName(id)] = struct{}{}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return false, "registry_unavailable", err
	}
	if err = rows.Close(); err != nil {
		return false, "registry_unavailable", err
	}
	var idsRows *sql.Rows
	idsRows, err = s.store.DB().QueryContext(ctx, `SELECT export_id FROM zoe_export_temp_registry WHERE workspace_id=? AND status='invalidated' AND invalidation_sequence<=? ORDER BY export_id`, workspace, sequence)
	if err != nil {
		return false, "registry_unavailable", err
	}
	for idsRows.Next() {
		var id string
		if err = idsRows.Scan(&id); err != nil {
			_ = idsRows.Close()
			return false, "registry_unavailable", err
		}
		targets = append(targets, id)
	}
	if err = idsRows.Err(); err != nil {
		_ = idsRows.Close()
		return false, "registry_unavailable", err
	}
	_ = idsRows.Close()
	directory, err := root.Open(".")
	if err != nil {
		return false, "unsafe_private_root", err
	}
	entries, err := directory.ReadDir(-1)
	closeErr := directory.Close()
	if err != nil {
		return false, "unsafe_private_root", err
	}
	if closeErr != nil {
		return false, "unsafe_private_root", closeErr
	}
	for _, entry := range entries {
		if _, ok := known[entry.Name()]; !ok {
			return false, "adapter_missing", errors.New("unregistered export artifact")
		}
	}
	for _, id := range targets {
		lock, ok := acquireExportTempLock(id, false)
		if !ok {
			return false, "export_active", nil
		}
		removeErr := removeRegisteredExportFiles(root, id)
		releaseExportTempLock(id, lock)
		if removeErr != nil {
			_, _ = s.store.DB().ExecContext(ctx, `UPDATE zoe_export_temp_registry SET last_error='unsafe_or_unremovable_artifact',updated_at=? WHERE export_id=?`, now, id)
			return false, "unsafe_or_unremovable_artifact", removeErr
		}
		if _, err = s.store.DB().ExecContext(ctx, `UPDATE zoe_export_temp_registry SET status='cleaned',updated_at=?,last_error='' WHERE export_id=? AND status='invalidated'`, now, id); err != nil {
			return false, "registry_unavailable", err
		}
	}
	return true, "", nil
}

func removeRegisteredExportFiles(root *os.Root, id string) error {
	for _, name := range []string{exportDataName(id), exportArchiveName(id)} {
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return errors.New("registered export artifact has unsafe type or permissions")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
			return errors.New("registered export artifact owner does not match control server")
		}
		if err = root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

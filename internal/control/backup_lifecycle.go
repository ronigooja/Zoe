package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ronigooja/Zoe/internal/storage"
)

const backupDeleteManifestVersion = 1

type backupDeleteManifestRecord struct {
	Version    int      `json:"version"`
	Sequence   int64    `json:"sequence"`
	Baseline   string   `json:"baseline"`
	Targets    []string `json:"targets"`
	SourceHash string   `json:"source_hash"`
}

// AppendBackupDeleteManifest durably records a deletion before the caller
// completes the corresponding cleanup job and advances its checkpoint.
func AppendBackupDeleteManifest(path string, record storage.DeletionRecord, trustedJournal []storage.DeletionRecord) error {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, string(filepath.Separator)+".."+string(filepath.Separator)) {
		return errors.New("backup deletion manifest path must be an explicit absolute path")
	}
	if record.Sequence < 1 || record.EntityID == "" || record.EntityVersion < 1 || !containsString(record.CleanupTargets, "backup_delete_manifest") {
		return errors.New("backup deletion manifest record is incomplete")
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateDirectory(parent); err != nil {
		return err
	}
	if existing, statErr := os.Lstat(path); statErr == nil {
		if !existing.Mode().IsRegular() || !ownedByCurrentUser(existing) || existing.Mode().Perm() != 0o600 {
			return errors.New("backup deletion manifest must be a current-user-owned regular file with mode 0600")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 || !ownedByCurrentUser(info) {
		return errors.New("backup deletion manifest must be a bounded regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("backup deletion manifest permissions are unsafe")
	}
	rows, err := readBackupDeleteManifest(file)
	if err != nil {
		return err
	}
	if err = validateBackupDeleteManifestRows(rows, trustedJournal); err != nil {
		return err
	}
	if record.Sequence > int64(len(trustedJournal)) || !matchesBackupDeleteRecord(trustedJournal[record.Sequence-1], backupDeleteManifestRecord{
		Version: backupDeleteManifestVersion, Sequence: record.Sequence, Baseline: record.LineageID,
		Targets: record.CleanupTargets, SourceHash: record.Hash,
	}) {
		return errors.New("backup deletion record does not match the trusted deletion journal")
	}
	for _, prior := range rows {
		if prior.Sequence != record.Sequence {
			continue
		}
		if prior.SourceHash == record.Hash && prior.Baseline == record.LineageID && sameStrings(prior.Targets, record.CleanupTargets) {
			return nil
		}
		return errors.New("backup deletion sequence is already bound to another record")
	}
	entry := backupDeleteManifestRecord{Version: backupDeleteManifestVersion, Sequence: record.Sequence, Baseline: record.LineageID, Targets: append([]string(nil), record.CleanupTargets...), SourceHash: record.Hash}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err = file.Write(append(line, '\n')); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	return syncDirectory(parent)
}

func validateBackupDeleteManifestRows(rows []backupDeleteManifestRecord, trustedJournal []storage.DeletionRecord) error {
	for _, row := range rows {
		if row.Sequence < 1 || row.Sequence > int64(len(trustedJournal)) || !matchesBackupDeleteRecord(trustedJournal[row.Sequence-1], row) {
			return errors.New("backup deletion manifest does not match the trusted deletion journal")
		}
	}
	return nil
}

func matchesBackupDeleteRecord(record storage.DeletionRecord, row backupDeleteManifestRecord) bool {
	return row.Version == backupDeleteManifestVersion && row.Sequence == record.Sequence && row.Baseline == record.LineageID && row.SourceHash == record.Hash && sameStrings(row.Targets, record.CleanupTargets)
}

func readBackupDeleteManifest(file *os.File) ([]backupDeleteManifestRecord, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	rows := []backupDeleteManifestRecord{}
	seen := map[int64]struct{}{}
	for scanner.Scan() {
		var row backupDeleteManifestRecord
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("backup deletion manifest record %d is invalid: %w", len(rows)+1, err)
		}
		if row.Version != backupDeleteManifestVersion || row.Sequence < 1 || row.Baseline == "" || row.SourceHash == "" || len(row.Targets) == 0 || !containsString(row.Targets, "backup_delete_manifest") {
			return nil, fmt.Errorf("backup deletion manifest chain is invalid at record %d", len(rows)+1)
		}
		if _, duplicate := seen[row.Sequence]; duplicate {
			return nil, fmt.Errorf("backup deletion manifest has duplicate sequence %d", row.Sequence)
		}
		seen[row.Sequence] = struct{}{}
		rows = append(rows, row)
	}
	return rows, scanner.Err()
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("backup deletion manifest directory cannot contain symlinks")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return errors.New("backup deletion manifest directory must be private")
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

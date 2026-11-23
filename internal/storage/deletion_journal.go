package storage

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DeletionRecord is the minimal durable replay instruction. It carries no
// deleted content and is stored outside SQLite backups beside the authority DB.
type DeletionRecord struct {
	Sequence       int64    `json:"sequence"`
	LineageID      string   `json:"lineage_id"`
	Generation     int64    `json:"generation"`
	EntityKind     string   `json:"entity_kind"`
	EntityID       string   `json:"entity_id"`
	EntityVersion  int64    `json:"entity_version"`
	ActorID        string   `json:"actor_id"`
	ReasonCode     string   `json:"reason_code"`
	DeletedAt      string   `json:"deleted_at"`
	CleanupTargets []string `json:"cleanup_targets"`
	PreviousHash   string   `json:"previous_hash"`
	Hash           string   `json:"hash"`
}

type deletionRecordBody struct {
	Sequence       int64    `json:"sequence"`
	LineageID      string   `json:"lineage_id"`
	Generation     int64    `json:"generation"`
	EntityKind     string   `json:"entity_kind"`
	EntityID       string   `json:"entity_id"`
	EntityVersion  int64    `json:"entity_version"`
	ActorID        string   `json:"actor_id"`
	ReasonCode     string   `json:"reason_code"`
	DeletedAt      string   `json:"deleted_at"`
	CleanupTargets []string `json:"cleanup_targets"`
	PreviousHash   string   `json:"previous_hash"`
}

type DeletionJournal struct{ path string }

func NewDeletionJournal(databasePath string) (*DeletionJournal, error) {
	if databasePath == "" || databasePath == ":memory:" {
		return nil, errors.New("deletion journal requires a file-backed authority database")
	}
	path, err := filepath.Abs(databasePath + ".deletion-ledger")
	if err != nil {
		return nil, err
	}
	return &DeletionJournal{path: path}, nil
}

func (j *DeletionJournal) Append(record DeletionRecord) (DeletionRecord, error) {
	if j == nil {
		return record, errors.New("independent deletion journal is unavailable")
	}
	if record.Sequence < 1 || strings.TrimSpace(record.LineageID) == "" || record.Generation < 1 || record.EntityKind == "" || record.EntityID == "" || record.EntityVersion < 1 {
		return record, errors.New("deletion journal record is incomplete")
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return record, err
	}
	_, statErr := os.Lstat(j.path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return record, statErr
	}
	if !created {
		info, err := os.Lstat(j.path)
		if err != nil {
			return record, err
		}
		if !info.Mode().IsRegular() {
			return record, errors.New("deletion journal must be a regular file")
		}
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_RDWR|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return record, err
	}
	defer f.Close()
	if err = f.Chmod(0o600); err != nil {
		return record, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return record, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	records, err := readDeletionRecords(f)
	if err != nil {
		return record, err
	}
	if record.Sequence <= int64(len(records)) {
		prior := records[record.Sequence-1]
		if prior.LineageID == record.LineageID && prior.Generation == record.Generation && prior.EntityKind == record.EntityKind && prior.EntityID == record.EntityID && prior.EntityVersion == record.EntityVersion {
			return prior, nil
		}
		return record, errors.New("deletion journal sequence is already bound to a different target")
	}
	if record.Sequence != int64(len(records))+1 {
		return record, fmt.Errorf("deletion journal gap: next sequence %d, record sequence %d", len(records)+1, record.Sequence)
	}
	if len(records) > 0 {
		last := records[len(records)-1]
		if last.LineageID != record.LineageID || record.Generation < last.Generation {
			return record, errors.New("deletion journal lineage or generation changed without a verified transition")
		}
		record.PreviousHash = last.Hash
	}
	body := deletionRecordBody{record.Sequence, record.LineageID, record.Generation, record.EntityKind, record.EntityID, record.EntityVersion, record.ActorID, record.ReasonCode, record.DeletedAt, append([]string(nil), record.CleanupTargets...), record.PreviousHash}
	encoded, err := json.Marshal(body)
	if err != nil {
		return record, err
	}
	digest := sha256.Sum256(encoded)
	record.Hash = hex.EncodeToString(digest[:])
	line, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	if _, err = f.Write(append(line, '\n')); err != nil {
		return record, err
	}
	if err = f.Sync(); err != nil {
		return record, err
	}
	if created {
		directory, openErr := os.Open(filepath.Dir(j.path))
		if openErr != nil {
			return record, openErr
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return record, syncErr
		}
		if closeErr != nil {
			return record, closeErr
		}
	}
	return record, nil
}

func (j *DeletionJournal) ReadAll() ([]DeletionRecord, error) {
	if j == nil {
		return nil, errors.New("independent deletion journal is unavailable")
	}
	f, err := os.OpenFile(j.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return []DeletionRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("deletion journal permissions or file type are unsafe")
	}
	return readDeletionRecords(f)
}

func readDeletionRecords(f *os.File) ([]DeletionRecord, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var records []DeletionRecord
	previousHash := ""
	lineage := ""
	previousGeneration := int64(0)
	for scanner.Scan() {
		var record DeletionRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("invalid deletion journal record %d: %w", len(records)+1, err)
		}
		if record.Sequence != int64(len(records)+1) || record.PreviousHash != previousHash || record.Hash == "" {
			return nil, fmt.Errorf("deletion journal sequence or hash chain gap at record %d", len(records)+1)
		}
		if strings.TrimSpace(record.LineageID) == "" || record.Generation < 1 || (lineage != "" && (record.LineageID != lineage || record.Generation < previousGeneration)) {
			return nil, fmt.Errorf("deletion journal lineage or generation gap at record %d", len(records)+1)
		}
		body := deletionRecordBody{record.Sequence, record.LineageID, record.Generation, record.EntityKind, record.EntityID, record.EntityVersion, record.ActorID, record.ReasonCode, record.DeletedAt, append([]string(nil), record.CleanupTargets...), record.PreviousHash}
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(encoded)
		if hex.EncodeToString(digest[:]) != record.Hash {
			return nil, fmt.Errorf("deletion journal hash mismatch at record %d", len(records)+1)
		}
		records = append(records, record)
		previousHash = record.Hash
		lineage = record.LineageID
		previousGeneration = record.Generation
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

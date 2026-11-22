package acceptance

import (
	"database/sql"
	"testing"
	"time"
)

func addMembershipDevice(t *testing.T, db *sql.DB, token, subject, deviceID string) {
	t.Helper()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO actors(id,subject_id,name,status,created_at,approved_at) VALUES(?, ?, 'peer', 'approved', ?, ?)`, deviceID, subject, at, at); err != nil {
		t.Fatal(err)
	}
}

package control

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/ronigooja/Zoe/contracts"
	"github.com/ronigooja/Zoe/internal/storage"
)

func TestExportFingerprintRetainsOwnerBoundaryInBothScopes(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.DB().Exec(`CREATE TABLE projects(id TEXT,status TEXT,owner_subject_id TEXT);
CREATE TABLE tasks(id TEXT,owner_subject_id TEXT);
INSERT INTO projects VALUES('mine','active','owner'),('other','active','other');
INSERT INTO tasks VALUES('mine','owner'),('other','other');`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p := contracts.Principal{SubjectID: "owner"}
	for _, scope := range []string{"personal", "instance"} {
		before, err := exportAuthorizationFingerprint(ctx, tx, p, scope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE projects SET id=id||'-changed' WHERE owner_subject_id='other';
UPDATE tasks SET id=id||'-changed' WHERE owner_subject_id='other'`); err != nil {
			t.Fatal(err)
		}
		after, err := exportAuthorizationFingerprint(ctx, tx, p, scope)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("%s fingerprint included another owner's rows", scope)
		}
		if _, err := tx.Exec(`UPDATE tasks SET owner_subject_id='other' WHERE owner_subject_id='owner'`); err != nil {
			t.Fatal(err)
		}
		revoked, err := exportAuthorizationFingerprint(ctx, tx, p, scope)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(before, revoked) {
			t.Fatalf("%s fingerprint missed task ownership revocation", scope)
		}
		if _, err := tx.Exec(`UPDATE tasks SET owner_subject_id='owner' WHERE id='mine'`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstanceExportCapabilityVisibilityRetainsOwnerBoundary(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.DB().Exec(`CREATE TABLE projects(id TEXT,owner_subject_id TEXT);
CREATE TABLE tasks(id TEXT,owner_subject_id TEXT);
CREATE TABLE capability_grants(grant_id TEXT,scope_kind TEXT,scope_id TEXT,principal_id TEXT,authorized_by TEXT);
INSERT INTO projects VALUES('mine','owner'),('other','other');
INSERT INTO tasks VALUES('mine','owner'),('other','other');
INSERT INTO capability_grants VALUES
('instance-mine','instance','','','owner'),('instance-other','instance','','','other'),
('personal-mine','personal','','owner','owner'),('personal-other','personal','','other','other'),
('project-mine','project','mine','','owner'),('project-other','project','other','','other'),
('task-mine','task','mine','','owner'),('task-other','task','other','','other');`)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"personal", "instance"} {
		rows, err := store.DB().Query(`SELECT g.grant_id FROM capability_grants g WHERE `+exportCapabilityVisibilityPredicate()+` ORDER BY g.grant_id`, "owner", scope)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		want := []string{"personal-mine"}
		if scope == "instance" {
			want = []string{"instance-mine", "personal-mine", "project-mine", "task-mine"}
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("%s grants=%v want=%v", scope, ids, want)
		}
	}
}

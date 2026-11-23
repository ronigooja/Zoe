package voice

import (
	"net/http"
	"testing"
)

func TestUIFreshBaselineConstraintsAndProductionRoute(t *testing.T) {
	ts, store, key := registerDisabledHTTPServer(t, Config{})
	for _, column := range []string{"utterance", "restatement"} {
		var n int
		if err := store.DB().QueryRow(`SELECT count(*) FROM pragma_table_info('ui_requests') WHERE name=?`, column).Scan(&n); err != nil || n != 0 {
			t.Fatalf("obsolete column %s count=%d err=%v", column, n, err)
		}
	}
	for _, assignment := range []string{"request=''", "request='none'", "surface_version=0", "status='feedback_delivered'"} {
		if _, err := store.DB().Exec(`INSERT INTO ui_requests(id,owner_subject_id,source,created_at,updated_at) VALUES('invalid','owner','register','now','now')`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`UPDATE ui_requests SET ` + assignment + ` WHERE id='invalid'`); err == nil {
			t.Fatalf("accepted invalid %s", assignment)
		}
		if _, err := store.DB().Exec(`DELETE FROM ui_requests WHERE id='invalid'`); err != nil {
			t.Fatal(err)
		}
	}
	for _, authorized := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/ui/snapshot", nil)
		if authorized {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := 401
		if authorized {
			want = 200
		}
		if response.StatusCode != want {
			t.Fatalf("production snapshot authorized=%v status=%d", authorized, response.StatusCode)
		}
	}
}

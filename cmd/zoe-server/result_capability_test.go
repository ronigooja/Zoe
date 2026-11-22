package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/internal/capabilities"
	"github.com/ronigooja/Zoe/internal/control"
	"github.com/ronigooja/Zoe/internal/storage"
	"github.com/ronigooja/Zoe/internal/voice"
)

func TestComposedReceiptSchedulerIndependentOfAccountMediaAndRegister(t *testing.T) {
	for _, mode := range []string{"supported", "unsupported", "no_gateway"} {
		t.Run(mode, func(t *testing.T) {
			var probes, sends atomic.Int32
			gw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer owner-key" {
					t.Error("missing gateway auth")
					w.WriteHeader(401)
					return
				}
				if r.URL.Path == "/v1/health" {
					probes.Add(1)
					if mode == "supported" {
						_, _ = w.Write([]byte(`{"result_deliveries":{"version":"v1","max_text_bytes":8000,"applied_evidence":"local_transport_writes_and_durable_receipt"}}`))
					} else {
						_, _ = w.Write([]byte(`{}`))
					}
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/voice/sessions/gw-s1/result-deliveries" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				sends.Add(1)
				var chunk voice.ResultChunkRequest
				if err := json.NewDecoder(r.Body).Decode(&chunk); err != nil {
					t.Error(err)
				}
				_ = json.NewEncoder(w).Encode(voice.ResultChunkReceipt{MediaGeneration: chunk.MediaGeneration, DeliveryID: chunk.DeliveryID, ContentHash: chunk.ContentHash, State: "applied"})
			}))
			defer gw.Close()
			store, err := storage.Open(context.Background(), storage.Config{Path: ":memory:"})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := storage.RegisterCoreMigrations(store); err != nil {
				t.Fatal(err)
			}
			cfg := voice.Config{DiscoverResultReceipts: true, GatewayURL: gw.URL, GatewayAPIKey: "owner-key", HTTPClient: gw.Client(), ResultDeliveryInterval: 10 * time.Millisecond}
			if mode == "no_gateway" {
				cfg.GatewayURL = ""
				cfg.GatewayAPIKey = ""
			}
			server, err := composeControlServer(store, control.Config{}, cfg, capabilities.Config{})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Shutdown()
			if err := store.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{
				`INSERT INTO actors(id,subject_id,name,status,created_at) VALUES('device-1','user-1','test','approved','2026-01-01')`,
				`INSERT INTO command_receipts(command_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES('cmd-1','user-1','key-1',X'01','{}','2026-01-01')`,
				`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,result,error,created_by_device_id,command_id,owner_subject_id) VALUES('task-1','default','title','goal','voice','zoe-steward','awaiting_user',1,'2026-01-01','2026-01-01','','','device-1','cmd-1','user-1')`,
				`INSERT INTO task_entries(id,task_id,workspace_id,kind,content,author_device_id,created_at) VALUES('entry-1','task-1','default','assistant_message','full result','device-1','2026-01-01')`,
				`INSERT INTO events(event_id,workspace_id,cursor,schema_version,event_type,occurred_at,aggregate_type,aggregate_id,aggregate_version,payload) VALUES('event-1','default',1,1,'task.assistant_message','2026-01-01','task','task-1',1,'{"payload":{"task_id":"task-1","run_id":"run-1","entry_id":"entry-1"}}')`,
				`INSERT INTO voice_sessions(id,workspace_id,owner_id,device_id,request_id,request_hash,gateway_session_id,generation,gateway_media_generation,state,created_at,updated_at) VALUES('s1','default','user-1','device-1','req-1',X'01','gw-s1',1,1,'active','2026-01-01','2026-01-01')`,
				// The live scheduler may already have created this delivery from event-1.
				`INSERT OR IGNORE INTO voice_result_deliveries(entry_id,workspace_id,task_id,run_id,subject_id,status,next_attempt_at,created_at,updated_at) VALUES('entry-1','default','task-1','run-1','user-1','pending','2026-01-01','2026-01-01','2026-01-01')`,
			} {
				if _, err := store.DB().Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(2 * time.Second)
			var status, code string
			for {
				if err := store.DB().QueryRow(`SELECT status,last_error_code FROM voice_result_deliveries WHERE entry_id='entry-1'`).Scan(&status, &code); err != nil {
					t.Fatal(err)
				}
				if mode == "supported" && status == "delivered" || mode == "unsupported" && code == "receipt_capability_disabled" || mode == "no_gateway" && time.Until(deadline) < 1800*time.Millisecond {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("scheduler did not progress status=%s code=%s", status, code)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "supported" {
				if sends.Load() != 1 || probes.Load() != 1 {
					t.Fatalf("sends=%d probes=%d", sends.Load(), probes.Load())
				}
			} else if status != "pending" || sends.Load() != 0 {
				t.Fatalf("status=%s sends=%d", status, sends.Load())
			}
			if mode == "no_gateway" && (probes.Load() != 0 || code != "") {
				t.Fatalf("disabled scheduler probes=%d code=%s", probes.Load(), code)
			}
		})
	}
}

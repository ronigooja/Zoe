package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

func TestAVASOpeningContextCreateItemsAndDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, acknowledgement, delivery string
		memory                          bool
	}{
		{"empty-delivered", `{"applied":true,"silent":true}`, "delivered", false},
		{"empty-unverified", "", "unverified", false},
		{"memory-delivered", `{"applied":true,"silent":true}`, "delivered", true},
		{"memory-unverified", "", "unverified", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received gatewaySessionRequest
			gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/voice/sessions" || r.Method != http.MethodPost {
					t.Errorf("unexpected gateway request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotImplemented)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				response := map[string]any{"answer_sdp": "answer", "voice_session_id": "gateway"}
				if tc.acknowledgement != "" {
					response["opening_context"] = json.RawMessage(tc.acknowledgement)
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer gateway.Close()
			store, svc := newVoiceTestService(t, Config{GatewayURL: gateway.URL, GatewayAPIKey: "key", HTTPClient: gateway.Client(), UpstreamRoute: "avas", ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
				if !tc.memory {
					return contracts.MemoryContext{}, nil
				}
				return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Subject: "internal-subject-id", Key: "喜欢的饮料", Content: "茶"}, {Subject: "internal-subject-id", Key: "居住地", Content: "上海"}}}, nil
			}})
			created, err := svc.create(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user", DeviceID: "device"}, createRequest{RequestID: "opening", OfferSDP: "offer"})
			if err != nil {
				t.Fatal(err)
			}
			// Only memory facts are emitted; no delegation rule is injected.
			want := []openingContextItem{}
			if tc.memory {
				want = append(want, openingContextItem{Role: "user", Text: "喜欢的饮料:茶"}, openingContextItem{Role: "user", Text: "居住地:上海"})
			}
			if received.OpeningContext == nil || !reflect.DeepEqual(received.OpeningContext.Items, want) {
				t.Fatalf("opening=%+v want=%+v", received.OpeningContext, want)
			}
			for _, item := range received.OpeningContext.Items {
				if strings.Contains(item.Text, "先委托") || strings.Contains(item.Text, "不要自己回答") {
					t.Fatalf("delegation rule emitted: %q", item.Text)
				}
			}
			var delivery string
			var manifest []byte
			var chars int
			if err := store.DB().QueryRow(`SELECT opening_context_delivery,opening_context_manifest,opening_context_chars FROM voice_sessions WHERE id=?`, created.SessionID).Scan(&delivery, &manifest, &chars); err != nil {
				t.Fatal(err)
			}
			if delivery != tc.delivery || created.OpeningContextDelivery != tc.delivery {
				t.Fatalf("delivery=%s response=%s want=%s", delivery, created.OpeningContextDelivery, tc.delivery)
			}
			var persisted openingContext
			if err := json.Unmarshal(manifest, &persisted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted.Items, want) || chars != openingCharsFor(received.OpeningContext) {
				t.Fatalf("persisted=%+v chars=%d", persisted, chars)
			}
		})
	}
}

func TestAVASOpeningContextEmptyWithoutMemoryOrBudget(t *testing.T) {
	for _, mode := range []string{"nil", "error", "empty", "facts"} {
		t.Run(mode, func(t *testing.T) {
			_, svc := newVoiceTestService(t, Config{UpstreamRoute: "avas", OpeningContextBudget: 1})
			if mode != "nil" {
				svc.contextAssembler = func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
					if mode == "error" {
						return contracts.MemoryContext{}, errors.New("unavailable")
					}
					if mode == "facts" {
						return contracts.MemoryContext{Items: []contracts.MemoryContextItem{{Key: "饮料", Content: "茶"}}}, nil
					}
					return contracts.MemoryContext{}, nil
				}
			}
			opening := svc.buildOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user"})
			if opening == nil || !reflect.DeepEqual(opening.Items, []openingContextItem{}) || openingCharsFor(opening) != 0 {
				t.Fatalf("opening=%+v", opening)
			}
			wire, err := json.Marshal(opening)
			if err != nil || !strings.Contains(string(wire), `"items":[]`) {
				t.Fatalf("empty opening context must encode an items array: %s err=%v", wire, err)
			}
		})
	}
}

func TestAVASOpeningContextFactBudgetAndLimit(t *testing.T) {
	_, svc := newVoiceTestService(t, Config{UpstreamRoute: "avas", OpeningContextBudget: 10000, ContextAssembler: func(context.Context, contracts.Principal, string, string, string, int) (contracts.MemoryContext, error) {
		items := make([]contracts.MemoryContextItem, 140)
		for i := range items {
			items[i] = contracts.MemoryContextItem{Subject: "internal-id", Key: "偏好", Content: fmt.Sprint(i)}
		}
		return contracts.MemoryContext{Items: items}, nil
	}})
	p := contracts.Principal{WorkspaceID: "default", SubjectID: "user"}
	opening := svc.buildOpeningContext(context.Background(), p)
	if len(opening.Items) != 5 {
		t.Fatalf("items=%d want five facts", len(opening.Items))
	}
	svc.openingBudget = len([]rune("偏好:0"))
	opening = svc.buildOpeningContext(context.Background(), p)
	if len(opening.Items) != 1 || opening.Items[0].Text != "偏好:0" || openingCharsFor(opening) != svc.openingBudget {
		t.Fatalf("opening=%+v chars=%d budget=%d", opening, openingCharsFor(opening), svc.openingBudget)
	}
}

func TestAVASOpeningContextRealNonTerminalTasks(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{UpstreamRoute: "avas", OpeningContextBudget: 2000})
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, owner := range []string{"user", "other"} {
		if _, err := store.DB().Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES(?,?,?,?,?,?)`, "device-"+owner, "default", owner, owner, "approved", now); err != nil {
			t.Fatal(err)
		}
	}
	for i, status := range []string{"active", "queued", "waiting", "awaiting_user", "paused", "complete", "failed", "canceled", "active"} {
		owner := "user"
		if i == 8 {
			owner = "other"
		}
		id := fmt.Sprintf("task-%d", i)
		if _, err := store.DB().Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,?,?,X'01','{}',?)`, "cmd-"+id, "default", owner, "key-"+id, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, "default", "任务"+fmt.Sprint(i), "目标", "voice", "default", status, 1, now, now, "device-"+owner, "cmd-"+id, owner); err != nil {
			t.Fatal(err)
		}
	}
	opening := svc.buildOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user"})
	want := []openingContextItem{}
	for _, i := range []int{0, 1, 2, 3} {
		want = append(want, openingContextItem{Role: "user", Text: "进行中的任务:任务" + fmt.Sprint(i)})
	}
	if opening == nil || !reflect.DeepEqual(opening.Items, want) {
		t.Fatalf("opening=%+v want=%+v", opening, want)
	}
	for _, item := range opening.Items {
		if strings.Contains(item.Text, "active task:") {
			t.Fatalf("English task label: %s", item.Text)
		}
	}
}

func TestAVASOpeningContextSkipsStaleWaitingTasksAndDuplicates(t *testing.T) {
	store, svc := newVoiceTestService(t, Config{UpstreamRoute: "avas", OpeningContextBudget: 2000})
	nowT := time.Now().UTC()
	fresh := nowT.Format(time.RFC3339Nano)
	stale := nowT.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := store.DB().Exec(`INSERT INTO actors(id,workspace_id,subject_id,name,status,created_at) VALUES('device-user','default','user','user','approved',?)`, fresh); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ status, title, updated string }{
		{"awaiting_user", "旧的等待任务", stale},
		{"waiting", "旧的等待任务二", stale},
		{"awaiting_user", "刚更新的等待任务", fresh},
		{"active", "重复任务", fresh},
		{"active", "重复任务", fresh},
		{"queued", "排队任务", fresh},
	}
	for i, r := range rows {
		id := fmt.Sprintf("stale-%d", i)
		if _, err := store.DB().Exec(`INSERT INTO command_receipts(command_id,workspace_id,subject_id,idempotency_key,request_hash,receipt_json,accepted_at) VALUES(?,?,?,?,X'01','{}',?)`, "cmd-"+id, "default", "user", "key-"+id, fresh); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO tasks(id,workspace_id,title,goal,source,profile_id,status,version,created_at,updated_at,created_by_device_id,command_id,owner_subject_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, "default", r.title, "目标", "voice", "default", r.status, 1, r.updated, r.updated, "device-user", "cmd-"+id, "user"); err != nil {
			t.Fatal(err)
		}
	}
	opening := svc.buildOpeningContext(context.Background(), contracts.Principal{WorkspaceID: "default", SubjectID: "user"})
	if opening == nil {
		t.Fatal("no opening context")
	}
	got := map[string]int{}
	for _, item := range opening.Items {
		got[item.Text]++
	}
	want := map[string]int{"进行中的任务:刚更新的等待任务": 1, "进行中的任务:重复任务": 1, "进行中的任务:排队任务": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items=%v want=%v", got, want)
	}
}

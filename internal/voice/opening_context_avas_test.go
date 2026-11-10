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
			// Literal expectation deliberately protects the exact mandatory wording.
			want := []openingContextItem{{Role: "user", Text: "用户要求取消或停止进行中的任务时,先委托,不要自己回答。用户询问任务进度或结果时,先委托,不要自己回答。用户询问关于他本人的任何信息时,先委托,等结果再回答,不要自己回答。"}}
			if tc.memory {
				want = append(want, openingContextItem{Role: "user", Text: "喜欢的饮料:茶"}, openingContextItem{Role: "user", Text: "居住地:上海"})
			}
			if received.OpeningContext == nil || !reflect.DeepEqual(received.OpeningContext.Items, want) {
				t.Fatalf("opening=%+v want=%+v", received.OpeningContext, want)
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

func TestAVASOpeningContextRuleSurvivesMissingMemoryAndBudget(t *testing.T) {
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
			if opening == nil || !reflect.DeepEqual(opening.Items, []openingContextItem{{Role: "user", Text: delegationRuleItem}}) {
				t.Fatalf("opening=%+v", opening)
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
	if len(opening.Items) != 6 {
		t.Fatalf("items=%d want rule plus five facts", len(opening.Items))
	}
	svc.openingBudget = len([]rune(delegationRuleItem)) + len([]rune("偏好:0"))
	opening = svc.buildOpeningContext(context.Background(), p)
	if len(opening.Items) != 2 || openingCharsFor(opening) != svc.openingBudget {
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
	want := []openingContextItem{{Role: "user", Text: delegationRuleItem}}
	for i := 0; i < 5; i++ {
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

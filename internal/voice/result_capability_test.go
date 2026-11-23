package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const supportedResultHealth = `{"result_deliveries":{"version":"v1","max_text_bytes":8000,"applied_evidence":"local_transport_writes_and_durable_receipt"}}`

func capabilityRig(t *testing.T, health func(http.ResponseWriter, *http.Request)) (*deliveryRig, *atomic.Int32) {
	t.Helper()
	r := checkpointRig(t)
	posts := &atomic.Int32{}
	gw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer capability-owner" {
			t.Error("missing owner authentication")
			w.WriteHeader(401)
			return
		}
		if req.URL.Path == "/v1/health" {
			if req.Method != http.MethodGet {
				t.Error("health method")
			}
			health(w, req)
			return
		}
		if req.Method != http.MethodPost || req.URL.Path != "/v1/voice/sessions/gw-s1/result-deliveries" {
			t.Errorf("unexpected transport %s %s", req.Method, req.URL.Path)
			w.WriteHeader(404)
			return
		}
		posts.Add(1)
		var p ResultChunkRequest
		if err := json.NewDecoder(req.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(ResultChunkReceipt{p.MediaGeneration, p.DeliveryID, p.ContentHash, "applied"})
	}))
	t.Cleanup(gw.Close)
	r.svc = newService(r.store.DB(), Config{GatewayURL: gw.URL, GatewayAPIKey: "capability-owner", HTTPClient: gw.Client(), Now: r.now, DiscoverResultReceipts: true})
	t.Cleanup(r.svc.Stop)
	r.setup(t, strings.Repeat("full result ", 1500))
	r.seedCall(t, "s1", "user-1", 1)
	r.exec(t, `UPDATE voice_sessions SET gateway_media_generation=1 WHERE id='s1'`)
	return r, posts
}

func TestResultCapabilityActivation(t *testing.T) {
	var probes atomic.Int32
	r, posts := capabilityRig(t, func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		_, _ = w.Write([]byte(supportedResultHealth))
	})
	r.run(t)
	status, code, attempts := r.row(t, "entry-1")
	if status != "delivered" || code != "gateway_accepted" || attempts != 1 || posts.Load() != 3 {
		t.Fatalf("%s %s attempts=%d posts=%d", status, code, attempts, posts.Load())
	}
	ok, code := r.svc.deliverDurableText(context.Background(), "user-1", "fast_reply", "fast-1", "s1", 1, "generic durable text")
	if !ok || code != "gateway_accepted" || posts.Load() != 4 || probes.Load() != 1 {
		t.Fatalf("generic=%v %s posts=%d probes=%d", ok, code, posts.Load(), probes.Load())
	}
}

func TestResultCapabilityUnsupportedRetainsPending(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{`, `{"result_deliveries":null}`, strings.Replace(supportedResultHealth, `"v1"`, `"v2"`, 1), strings.Replace(supportedResultHealth, `8000`, `7999`, 1), strings.Replace(supportedResultHealth, `local_transport_writes_and_durable_receipt`, `speech`, 1), strings.Replace(supportedResultHealth, `"version":`, `"unknown":true,"version":`, 1), `{"result_deliveries":{"version":"v1","max_text_bytes":"8000","applied_evidence":"local_transport_writes_and_durable_receipt"}}`, "unavailable"} {
		t.Run(body, func(t *testing.T) {
			var probes atomic.Int32
			r, posts := capabilityRig(t, func(w http.ResponseWriter, _ *http.Request) {
				probes.Add(1)
				if body == "unavailable" {
					w.WriteHeader(503)
				}
				_, _ = w.Write([]byte(body))
			})
			r.run(t)
			status, code, attempts := r.row(t, "entry-1")
			if status != "pending" || code != "receipt_capability_disabled" || attempts != 0 || posts.Load() != 0 {
				t.Fatalf("%s %s attempts=%d posts=%d", status, code, attempts, posts.Load())
			}
			ok, code := r.svc.deliverDurableText(context.Background(), "user-1", "fast_reply", "fast-1", "s1", 1, "generic")
			if ok || code != "receipt_capability_disabled" || probes.Load() != 1 {
				t.Fatalf("%v %s probes=%d", ok, code, probes.Load())
			}
		})
	}
}

func TestResultCapabilityRefreshSingleProbe(t *testing.T) {
	var probes atomic.Int32
	var enabled atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	r, posts := capabilityRig(t, func(w http.ResponseWriter, _ *http.Request) {
		n := probes.Add(1)
		if n == 2 {
			close(entered)
			<-release
		}
		if enabled.Load() {
			_, _ = w.Write([]byte(supportedResultHealth))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	})
	r.run(t)
	enabled.Store(true)
	r.advance(resultCapabilityRefresh + time.Second)
	done := make(chan struct{})
	go func() { defer close(done); r.svc.receiptAdapter(context.Background()) }()
	<-entered
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.svc.receiptAdapter(context.Background()) != nil {
				t.Error("stale capability used during probe")
			}
		}()
	}
	wg.Wait()
	close(release)
	<-done
	r.run(t)
	if probes.Load() != 2 || posts.Load() != 3 {
		t.Fatalf("probes=%d posts=%d", probes.Load(), posts.Load())
	}
	enabled.Store(false)
	r.advance(resultCapabilityRefresh + time.Second)
	if r.svc.receiptAdapter(context.Background()) != nil || probes.Load() != 3 {
		t.Fatal("removed capability remained active")
	}
}

func TestResultCapabilityInjectionPrecedence(t *testing.T) {
	a := &checkpointAdapter{}
	s := newService(nil, Config{DiscoverResultReceipts: true, ResultDelivery: ResultDeliveryConfig{ReceiptAdapter: a}})
	defer s.Stop()
	if s.receiptAdapter(context.Background()) != a || s.resultCapability != nil {
		t.Fatal("explicit injection overridden")
	}
}

func TestResultCapabilitySchedulerRequiresConfiguredGateway(t *testing.T) {
	for _, cfg := range []Config{
		{DiscoverResultReceipts: true},
		{DiscoverResultReceipts: true, GatewayURL: "https://gateway.invalid"},
		{DiscoverResultReceipts: true, GatewayAPIKey: "owner-key"},
		{DiscoverResultReceipts: true, GatewayURL: "http://gateway.invalid", GatewayAPIKey: "owner-key"},
		{GatewayURL: "https://gateway.invalid", GatewayAPIKey: "owner-key"},
	} {
		s := newService(nil, cfg)
		if s.resultDeliveryEnabled || s.resultCapability != nil || s.receiptAdapter(context.Background()) != nil {
			t.Fatal("disabled/unconfigured discovery enabled scheduler or adapter")
		}
		s.Stop()
	}
	s := newService(nil, Config{DiscoverResultReceipts: true, GatewayURL: "https://gateway.invalid", GatewayAPIKey: "owner-key"})
	defer s.Stop()
	if !s.resultDeliveryEnabled || s.resultCapability == nil {
		t.Fatal("configured discovery did not enable independent scheduler")
	}
}

func TestResultCapabilityProbeDeadlineAndMalformed(t *testing.T) {
	for _, data := range []string{
		`{"result_deliveries":{"version":"v2","version":"v1","max_text_bytes":8000,"applied_evidence":"local_transport_writes_and_durable_receipt"}}`,
		strings.TrimSuffix(supportedResultHealth, "}") + `,"result_deliveries":{}}`,
		supportedResultHealth + `{}`,
		strings.Repeat(" ", 65537) + supportedResultHealth,
	} {
		r, posts := capabilityRig(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(data)) })
		r.run(t)
		if r.svc.receiptAdapter(context.Background()) != nil || posts.Load() != 0 {
			t.Fatal("malformed health enabled sends")
		}
	}
	r, posts := capabilityRig(t, func(_ http.ResponseWriter, req *http.Request) { <-req.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if r.svc.receiptAdapter(ctx) != nil || posts.Load() != 0 || time.Since(start) > time.Second {
		t.Fatal("probe ignored caller budget")
	}
	if r.svc.receiptAdapter(context.Background()) != nil {
		t.Fatal("failed probe bypassed cache")
	}
}

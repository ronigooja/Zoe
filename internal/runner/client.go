package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/contracts"
)

type ControlClient struct {
	base   *url.URL
	token  string
	client *http.Client
}

func (c *ControlClient) AcknowledgeEntry(ctx context.Context, runID string, delivery contracts.EntryDelivery) error {
	b, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	r, err := c.request(ctx, http.MethodPost, "/runner/v1/runs/"+url.PathEscape(runID)+"/entry-deliveries", b, "")
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		return safeHTTPError("acknowledge entry", r.StatusCode)
	}
	return nil
}

func (c *ControlClient) RotationContext(ctx context.Context, runID, attemptID string, fence uint64) (contracts.RotationContext, error) {
	return c.RotationSnapshot(ctx, runID, attemptID, fence, "")
}

func (c *ControlClient) RotationSnapshot(ctx context.Context, runID, attemptID string, fence uint64, snapshotID string) (contracts.RotationContext, error) {
	endpoint := "/runner/v1/runs/" + url.PathEscape(runID) + "/rotation-context?attempt_id=" + url.QueryEscape(attemptID) + "&fence=" + url.QueryEscape(fmt.Sprint(fence))
	if snapshotID != "" {
		endpoint += "&snapshot_id=" + url.QueryEscape(snapshotID)
	}
	response, err := c.request(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return contracts.RotationContext{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return contracts.RotationContext{}, safeHTTPError("read rotation context", response.StatusCode)
	}
	var out contracts.RotationContext
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&out); err != nil {
		return out, fmt.Errorf("decode rotation context: %w", err)
	}
	return out, nil
}

func (c *ControlClient) RuntimeAuthorization(ctx context.Context, runID, requestID string, fence uint64) (*contracts.RuntimeAuthorization, error) {
	endpoint := "/runner/v1/runs/" + url.PathEscape(runID) + "/authorizations/" + url.PathEscape(requestID) + "?fence=" + url.QueryEscape(fmt.Sprint(fence))
	response, err := c.request(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, safeHTTPError("read runtime authorization", response.StatusCode)
	}
	var out contracts.RuntimeAuthorization
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InstanceState is the durable host epoch used to fence external runner
// effects across backup/restore and deployment handoff.
type InstanceState struct {
	InstanceID      string `json:"instance_id"`
	Epoch           int64  `json:"epoch"`
	DispatchEnabled bool   `json:"dispatch_enabled"`
}

func NewControlClient(baseURL, token string, client *http.Client) (*ControlClient, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("control URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("runner token is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &ControlClient{base: u, token: token, client: client}, nil
}

func (c *ControlClient) Claim(ctx context.Context) (*contracts.JobClaim, error) {
	response, err := c.request(ctx, http.MethodPost, "/runner/v1/claims", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, controlHTTPError{operation: "claim", status: response.StatusCode}
	}
	var claim contracts.JobClaim
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&claim); err != nil {
		return nil, controlDecodeError{fmt.Errorf("decode runner claim: %w", err)}
	}
	return &claim, nil
}

func (c *ControlClient) Instance(ctx context.Context) (InstanceState, error) {
	response, err := c.request(ctx, http.MethodGet, "/runner/v1/instance", nil, "")
	if err != nil {
		return InstanceState{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return InstanceState{}, safeHTTPError("read active instance", response.StatusCode)
	}
	var state InstanceState
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&state); err != nil {
		return InstanceState{}, fmt.Errorf("decode runner instance state: %w", err)
	}
	if strings.TrimSpace(state.InstanceID) == "" || state.Epoch < 1 {
		return InstanceState{}, errors.New("runner instance state is incomplete")
	}
	return state, nil
}

func (c *ControlClient) Current(ctx context.Context) ([]contracts.JobClaim, error) {
	response, err := c.request(ctx, http.MethodGet, "/runner/v1/claims/current", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, controlHTTPError{operation: "read current claims", status: response.StatusCode}
	}
	var body struct {
		Claims []contracts.JobClaim `json:"claims"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&body); err != nil {
		return nil, controlDecodeError{fmt.Errorf("decode current claims: %w", err)}
	}
	return body.Claims, nil
}

func (c *ControlClient) Control(ctx context.Context, runID string) (contracts.RunControl, error) {
	response, err := c.request(ctx, http.MethodGet, "/runner/v1/runs/"+url.PathEscape(runID), nil, "")
	if err != nil {
		return contracts.RunControl{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return contracts.RunControl{}, safeHTTPError("read run control", response.StatusCode)
	}
	var control contracts.RunControl
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&control); err != nil {
		return contracts.RunControl{}, fmt.Errorf("decode run control: %w", err)
	}
	return control, nil
}

// CheckDispatchAuthorization refreshes the source grant immediately before an
// Runtime create or exact-request recovery. A denied check after dispatch_started
// must remain uncertain; it does not prove that the earlier request had no
// effect.
func (c *ControlClient) CheckDispatchAuthorization(ctx context.Context, runID string) (bool, string, error) {
	if strings.TrimSpace(runID) == "" {
		return false, "", errors.New("run id is required for dispatch authorization")
	}
	response, err := c.request(ctx, http.MethodPost, "/runner/v1/runs/"+url.PathEscape(runID)+"/dispatch-authorization", nil, "")
	if err != nil {
		return false, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, "", safeHTTPError("check task dispatch authorization", response.StatusCode)
	}
	var result contracts.DispatchAuthorization
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
		return false, "", fmt.Errorf("decode task dispatch authorization: %w", err)
	}
	return result.Allowed, result.ReasonCode, nil
}

func (c *ControlClient) ReserveBudget(ctx context.Context, runID string, request contracts.RunBudgetReserveRequest) (contracts.RunBudgetReceipt, error) {
	if runID == "" || request.ReservationID == "" {
		return contracts.RunBudgetReceipt{}, errors.New("run and budget reservation ids are required")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return contracts.RunBudgetReceipt{}, fmt.Errorf("encode run budget reservation: %w", err)
	}
	endpoint := "/runner/v1/runs/" + url.PathEscape(runID) + "/budget/reservations"
	response, err := c.request(ctx, http.MethodPost, endpoint, encoded, request.ReservationID)
	if err != nil {
		return contracts.RunBudgetReceipt{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return contracts.RunBudgetReceipt{}, safeHTTPError("reserve run budget", response.StatusCode)
	}
	var receipt contracts.RunBudgetReceipt
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("decode run budget reservation: %w", err)
	}
	return receipt, nil
}

func (c *ControlClient) SettleBudget(ctx context.Context, runID, reservationID string, request contracts.RunBudgetSettleRequest) (contracts.RunBudgetReceipt, error) {
	if runID == "" || reservationID == "" || request.SettlementID == "" {
		return contracts.RunBudgetReceipt{}, errors.New("run, reservation, and settlement ids are required")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return contracts.RunBudgetReceipt{}, fmt.Errorf("encode run budget settlement: %w", err)
	}
	endpoint := "/runner/v1/runs/" + url.PathEscape(runID) + "/budget/reservations/" + url.PathEscape(reservationID) + "/settlement"
	response, err := c.request(ctx, http.MethodPost, endpoint, encoded, request.SettlementID)
	if err != nil {
		return contracts.RunBudgetReceipt{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return contracts.RunBudgetReceipt{}, safeHTTPError("settle run budget", response.StatusCode)
	}
	var receipt contracts.RunBudgetReceipt
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("decode run budget settlement: %w", err)
	}
	return receipt, nil
}

func (c *ControlClient) Observe(ctx context.Context, runID string, observation contracts.RunnerObservation) error {
	if observation.ObservationID == "" {
		return errors.New("observation id is required")
	}
	if runID == "" {
		return errors.New("run id is required")
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return fmt.Errorf("encode runner observation: %w", err)
	}
	endpoint := "/runner/v1/runs/" + url.PathEscape(runID) + "/observations"
	response, err := c.request(ctx, http.MethodPost, endpoint, encoded, observation.ObservationID)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		var apiErr contracts.ErrorEnvelope
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Error.Code != "" {
			return fmt.Errorf("runner API submit observation failed (HTTP %d, %s)", response.StatusCode, apiErr.Error.Code)
		}
		return safeHTTPError("submit observation", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (c *ControlClient) ClaimDeployment(ctx context.Context) (*contracts.DeploymentClaim, error) {
	response, err := c.request(ctx, http.MethodPost, "/runner/v1/deployments/claims", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, safeHTTPError("claim capability deployment", response.StatusCode)
	}
	var claim contracts.DeploymentClaim
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&claim); err != nil {
		return nil, fmt.Errorf("decode capability deployment claim: %w", err)
	}
	return &claim, nil
}

func (c *ControlClient) CurrentDeploymentClaim(ctx context.Context) (*contracts.DeploymentClaim, error) {
	response, err := c.request(ctx, http.MethodGet, "/runner/v1/deployments/claims/current", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, safeHTTPError("get current capability deployment claim", response.StatusCode)
	}
	var claim contracts.DeploymentClaim
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&claim); err != nil {
		return nil, fmt.Errorf("decode current capability deployment claim: %w", err)
	}
	return &claim, nil
}

func (c *ControlClient) UnresolvedDeploymentClaims(ctx context.Context) ([]contracts.DeploymentClaim, error) {
	response, err := c.request(ctx, http.MethodGet, "/runner/v1/deployments/claims/unresolved", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, safeHTTPError("list unresolved capability deployments", response.StatusCode)
	}
	var claims []contracts.DeploymentClaim
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&claims); err != nil {
		return nil, fmt.Errorf("decode unresolved capability deployment claims: %w", err)
	}
	return claims, nil
}

var _ DeploymentControl = (*ControlClient)(nil)

func (c *ControlClient) ObserveDeployment(ctx context.Context, observation contracts.DeploymentObservation) error {
	if observation.DeploymentID == "" || observation.ObservationID == "" {
		return errors.New("deployment and observation ids are required")
	}
	body, err := json.Marshal(observation)
	if err != nil {
		return fmt.Errorf("encode deployment observation: %w", err)
	}
	endpoint := "/runner/v1/deployments/" + url.PathEscape(observation.DeploymentID) + "/observations"
	response, err := c.request(ctx, http.MethodPost, endpoint, body, observation.ObservationID)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return safeHTTPError("submit capability deployment observation", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (c *ControlClient) request(ctx context.Context, method, endpoint string, body []byte, idempotencyKey string) (*http.Response, error) {
	u := *c.base
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(c.base.Path, "/") + parsed.Path
	u.RawQuery = parsed.RawQuery
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return c.client.Do(req)
}

func safeHTTPError(action string, status int) error {
	return fmt.Errorf("runner API %s failed (HTTP %d)", action, status)
}

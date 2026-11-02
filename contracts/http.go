package contracts

import (
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Principal is populated exclusively from authenticated server state.
type Principal struct {
	Generation     int64    `json:"-"`
	InstallationID string   `json:"-"`
	WorkspaceID    string   `json:"-"`
	SubjectID      string   `json:"-"`
	DeviceID       string   `json:"-"`
	Scopes         []string `json:"-"`
}

type RunnerPrincipal struct {
	RunnerID     string   `json:"-"`
	WorkspaceIDs []string `json:"-"`
	Scopes       []string `json:"-"`
}

type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type CommandRequest struct {
	Kind            string          `json:"kind"`
	ExpectedVersion *int64          `json:"expected_version,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

type CommandReceipt struct {
	CommandID       string    `json:"command_id"`
	IdempotencyKey  string    `json:"idempotency_key"`
	WorkspaceID     string    `json:"workspace_id"`
	Kind            string    `json:"kind"`
	Status          string    `json:"status"`
	ResourceType    string    `json:"resource_type,omitempty"`
	ResourceID      string    `json:"resource_id,omitempty"`
	ResourceVersion int64     `json:"resource_version,omitempty"`
	AcceptedAt      time.Time `json:"accepted_at"`
	EventCursor     string    `json:"event_cursor"`
}

type CommandResult struct {
	ResourceType    string
	ResourceID      string
	ResourceVersion int64
}

type Event struct {
	EventID          string          `json:"event_id"`
	Cursor           string          `json:"cursor"`
	SchemaVersion    int             `json:"schema_version"`
	Type             string          `json:"type"`
	OccurredAt       time.Time       `json:"occurred_at"`
	WorkspaceID      string          `json:"workspace_id"`
	SubjectID        string          `json:"subject_id,omitempty"`
	AggregateType    string          `json:"aggregate_type"`
	AggregateID      string          `json:"aggregate_id"`
	AggregateVersion int64           `json:"aggregate_version"`
	Payload          json.RawMessage `json:"payload"`
}

type Evidence struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Summary   string    `json:"summary"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	Verified  bool      `json:"verified"`
}

type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Status      string    `json:"status"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Task struct {
	QueueReason                string        `json:"queue_reason,omitempty"`
	ID                         string        `json:"id"`
	ProjectID                  string        `json:"project_id,omitempty"`
	Title                      string        `json:"title"`
	Goal                       string        `json:"goal"`
	Source                     string        `json:"source"`
	ProfileID                  string        `json:"profile_id,omitempty"`
	Status                     string        `json:"status"`
	Version                    int64         `json:"version"`
	CreatedAt                  time.Time     `json:"created_at"`
	UpdatedAt                  time.Time     `json:"updated_at"`
	AcceptedAt                 *time.Time    `json:"accepted_at,omitempty"`
	StartedAt                  *time.Time    `json:"started_at,omitempty"`
	FinishedAt                 *time.Time    `json:"finished_at,omitempty"`
	Result                     string        `json:"result,omitempty"`
	Error                      string        `json:"error,omitempty"`
	Evidence                   []Evidence    `json:"evidence"`
	CanAcceptResult            bool          `json:"can_accept_result"`
	InteractionState           string        `json:"interaction_state"`
	InteractionScope           string        `json:"interaction_scope,omitempty"`
	InteractionRunID           string        `json:"interaction_run_id,omitempty"`
	InteractionActionDigest    string        `json:"interaction_action_digest,omitempty"`
	InteractionExpectedVersion int64         `json:"interaction_expected_version,omitempty"`
	InteractionExpiresAt       *time.Time    `json:"interaction_expires_at,omitempty"`
	InteractionQuestion        string        `json:"interaction_question,omitempty"`
	Messages                   []TaskMessage `json:"messages,omitempty"`
}

type TaskEntry struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Content        string    `json:"content"`
	AuthorDeviceID string    `json:"author_device_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// TaskMessage is a task timeline entry returned by the paginated task history endpoint.
type TaskMessage struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Kind      string    `json:"kind"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Approval struct {
	ID                    string          `json:"id"`
	TaskID                string          `json:"task_id"`
	TaskVersion           int64           `json:"task_version"`
	Status                string          `json:"status"`
	Prompt                string          `json:"prompt"`
	Version               int64           `json:"version"`
	CreatedAt             time.Time       `json:"created_at"`
	ExpiresAt             *time.Time      `json:"expires_at,omitempty"`
	RequestedAction       json.RawMessage `json:"requested_action,omitempty"`
	RequestedActionDigest string          `json:"requested_action_digest,omitempty"`
}

// ConversationalConfirmation is the auditable server-owned authorization
// context for a high-impact action. It is never inferred from a result
// acceptance decision or from ambiguous conversational language.
type ConversationalConfirmation struct {
	TaskID          string    `json:"task_id"`
	RunID           string    `json:"run_id"`
	ActionDigest    string    `json:"action_digest"`
	Scope           string    `json:"scope"`
	ExpectedVersion int64     `json:"expected_version"`
	ExpiresAt       time.Time `json:"expires_at"`
	ConfirmedText   string    `json:"confirmed_text"`
	DeviceID        string    `json:"device_id"`
}

type Snapshot struct {
	ExecutionCapacity ExecutionCapacity `json:"execution_capacity"`
	WorkspaceID       string            `json:"workspace_id"`
	SubjectID         string            `json:"subject_id"`
	ServerVersion     string            `json:"server_version"`
	EventCursor       string            `json:"event_cursor"`
	Authority         AuthorityVersion  `json:"authority"`
	Projects          []Project         `json:"projects"`
	Tasks             []Task            `json:"tasks"`
	Approvals         []Approval        `json:"approvals"`
	Runs              []Run             `json:"runs"`
}

// ExecutionCapacity describes the global dispatch gate without exposing other tasks.
type ExecutionCapacity struct {
	ActiveRuns      int    `json:"active_runs"`
	UncertainRuns   int    `json:"uncertain_runs"`
	MaxRuns         int    `json:"max_runs"`
	AvailableSlots  int    `json:"available_slots"`
	DispatchBlocked bool   `json:"dispatch_blocked"`
	ReasonCode      string `json:"reason_code,omitempty"`
}

type AuthorityVersion struct {
	InstanceLineageID string `json:"instance_lineage_id"`
	Generation        int64  `json:"generation"`
	DeletionSequence  string `json:"deletion_sequence"`
}

type Notification struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	AggregateType string     `json:"aggregate_type"`
	AggregateID   string     `json:"aggregate_id"`
	SourceEventID string     `json:"source_event_id"`
	CreatedAt     time.Time  `json:"created_at"`
	ReadAt        *time.Time `json:"read_at,omitempty"`
}

type Run struct {
	ID                   string     `json:"id"`
	TaskID               string     `json:"task_id"`
	AttemptID            string     `json:"attempt_id"`
	Status               string     `json:"status"`
	DesiredControl       string     `json:"desired_control"`
	ControlVersion       int64      `json:"control_version"`
	StartedAt            time.Time  `json:"started_at"`
	StoppedAt            *time.Time `json:"stopped_at,omitempty"`
	TerminationConfirmed bool       `json:"termination_confirmed"`
	Evidence             []Evidence `json:"evidence"`
}

type JobAuthorization struct {
	CapabilityIDs   []string         `json:"capability_ids"`
	Scopes          []string         `json:"scopes"`
	PermissionTier  string           `json:"permission_tier,omitempty"`
	PermissionRules []PermissionRule `json:"permission_rules,omitempty"`
	ImpactLevel     string           `json:"impact_level,omitempty"`
}

type JobClaim struct {
	RunID                 string            `json:"run_id"`
	TaskID                string            `json:"task_id"`
	AttemptID             string            `json:"attempt_id"`
	WorkspaceID           string            `json:"workspace_id"`
	TaskVersion           int64             `json:"task_version"`
	Goal                  string            `json:"goal"`
	ProfileID             string            `json:"profile_id"`
	Harness               string            `json:"harness"`
	RuntimeRouteID        string            `json:"runtime_route_id"`
	ModelRouteID          string            `json:"model_route_id"`
	IsolationPolicy       string            `json:"isolation_policy"`
	Role                  string            `json:"role"`
	RuntimeSessionID      string            `json:"runtime_session_id,omitempty"`
	Authorization         JobAuthorization  `json:"authorization"`
	Fence                 uint64            `json:"fence"`
	LineageRootTaskID     string            `json:"lineage_root_task_id,omitempty"`
	LineageParentTaskID   string            `json:"lineage_parent_task_id,omitempty"`
	LineageDepth          int               `json:"lineage_depth"`
	PregrantCapabilityIDs []string          `json:"pregrant_capability_ids"`
	Budget                RunBudgetSnapshot `json:"budget"`
	Agent                 string            `json:"agent,omitempty"`
	PermissionTier        string            `json:"permission_tier,omitempty"`
	PermissionRules       []PermissionRule  `json:"permission_rules,omitempty"`
	ImpactLevel           string            `json:"impact_level,omitempty"`
}

// RunBudgetSnapshot is immutable for a run and is copied into its claim. Usage
// counters are enforced by the authoritative control-plane ledger.
type RunBudgetSnapshot struct {
	HardDeadline     time.Time `json:"hard_deadline"`
	MaxModelCalls    int64     `json:"max_model_calls"`
	MaxInputUnits    int64     `json:"max_input_units"`
	MaxOutputUnits   int64     `json:"max_output_units"`
	MaxToolSteps     int64     `json:"max_tool_steps"`
	MaxArtifactBytes int64     `json:"max_artifact_bytes"`
}

type ChildTaskBudget struct {
	MaxChildRuns int `json:"max_child_runs"`
	MaxDepth     int `json:"max_depth"`
}

type ChildTaskProposalRequest struct {
	ProposalID             string          `json:"proposal_id"`
	Attempt                string          `json:"attempt"`
	Fence                  uint64          `json:"fence"`
	Kind                   string          `json:"kind"`
	Title                  string          `json:"title"`
	Goal                   string          `json:"goal"`
	ProfileID              string          `json:"profile_id"`
	DependencyTaskIDs      []string        `json:"dependency_task_ids"`
	RequestedCapabilityIDs []string        `json:"requested_capability_ids"`
	Budget                 ChildTaskBudget `json:"budget"`
}

type ChildTaskProposalReceipt struct {
	ProposalID string `json:"proposal_id"`
	Status     string `json:"status"`
	Version    int64  `json:"version"`
	TaskID     string `json:"task_id,omitempty"`
}

type ClaimRecovery struct {
	Claims []JobClaim `json:"claims"`
}

type RunControl struct {
	RunID          string `json:"run_id"`
	DesiredControl string `json:"desired_control"`
	ControlVersion int64  `json:"control_version"`
}

type ExternalEffect struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Reference string `json:"reference,omitempty"`
}

type RunnerObservation struct {
	ObservationID       string                `json:"observation_id"`
	RunID               string                `json:"run_id"`
	AttemptID           string                `json:"attempt_id"`
	Fence               uint64                `json:"fence"`
	Kind                string                `json:"kind"`
	OccurredAt          time.Time             `json:"occurred_at"`
	RuntimeSessionID    string                `json:"runtime_session_id,omitempty"`
	RuntimeContainerID  string                `json:"runtime_container_id,omitempty"`
	Evidence            []Evidence            `json:"evidence"`
	Result              json.RawMessage       `json:"result,omitempty"`
	Error               string                `json:"error,omitempty"`
	ExternalEffects     []ExternalEffect      `json:"external_effects"`
	TerminationProof    *TerminationProof     `json:"termination_proof,omitempty"`
	NoEffectProof       *NoEffectProof        `json:"no_effect_proof,omitempty"`
	CreateRejection     *CreateRejectionProof `json:"create_rejection,omitempty"`
	BudgetReservationID string                `json:"budget_reservation_id,omitempty"`
}

type PendingAction struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	RequestID    string    `json:"request_id"`
	Action       string    `json:"action,omitempty"`
	Resources    []string  `json:"resources,omitempty"`
	Question     string    `json:"question,omitempty"`
	RunID        string    `json:"run_id"`
	Fence        uint64    `json:"fence"`
	ActionDigest string    `json:"action_digest"`
	ExpiresAt    time.Time `json:"expires_at"`
}
type RuntimeAuthorization struct {
	RequestID string `json:"request_id"`
	RunID     string `json:"run_id"`
	Fence     uint64 `json:"fence"`
	Decision  string `json:"decision"`
	Message   string `json:"message,omitempty"`
}

// CreateRejectionProof proves a local deterministic failure before an external
// runtime creation effect.
type CreateRejectionProof struct {
	Code string `json:"code"`
}

// NoEffectProof is the runner journal receipt for a run with no runtime.
// It proves either creation never began or explicit read-only reconciliation
// established absence under a newer runtime process. The control plane also checks
// its own observation and effect history before accepting this receipt.
type NoEffectProof struct {
	JournalPhase         string                  `json:"journal_phase"`
	RuntimeCreateStarted bool                    `json:"runtime_create_started"`
	RuntimeSessionID     string                  `json:"runtime_session_id,omitempty"`
	RuntimeContainerID   string                  `json:"runtime_container_id,omitempty"`
	Outcome              string                  `json:"outcome"`
	Reconciliation       *NoEffectReconciliation `json:"reconciliation,omitempty"`
}

// NoEffectReconciliation records explicit operator-authorized, read-only absence checks.
type NoEffectReconciliation struct {
	RunID                                      string                           `json:"run_id"`
	Actor                                      string                           `json:"actor"`
	Confirmed                                  bool                             `json:"confirmed"`
	VerifiedAt                                 time.Time                        `json:"verified_at"`
	JournalUpdatedAt                           time.Time                        `json:"journal_updated_at"`
	RuntimeQuery                               string                           `json:"runtime_query"`
	RuntimeInventoryComplete                   bool                             `json:"runtime_inventory_complete"`
	RuntimeMatches                             int                              `json:"runtime_matches"`
	DockerQuery                                string                           `json:"docker_query"`
	DockerInventoryComplete                    bool                             `json:"docker_inventory_complete"`
	DockerRuntimeCount                         int                              `json:"docker_runtime_count"`
	DockerInventoryContainerCount              int                              `json:"docker_inventory_container_count"`
	UnrelatedPreexistingCapabilityContainerIDs []string                         `json:"unrelated_preexisting_capability_container_ids,omitempty"`
	PreexistingCapabilityContainers            []PreexistingCapabilityContainer `json:"preexisting_capability_containers,omitempty"`
	DockerInventoryChecks                      int                              `json:"docker_inventory_checks"`
	RetainNeverStartedContainerIDs             []string                         `json:"retain_never_started_container_ids,omitempty"`
	RetainedNeverStartedContainers             []RetainedNeverStartedContainer  `json:"retained_never_started_containers,omitempty"`
	UnmanagedContainerIDs                      []string                         `json:"unmanaged_container_ids,omitempty"`
	UnrelatedCapabilityContainerIDs            []string                         `json:"unrelated_capability_container_ids,omitempty"`

	InstanceID    string `json:"instance_id"`
	InstanceEpoch int64  `json:"instance_epoch"`
}

// RetainedNeverStartedContainer preserves an explicitly selected Docker artifact
// while proving that it has never launched a process. Pointer fields make absent
// or null inspect data invalid, rather than treating it as a zero value.
type RetainedNeverStartedContainer struct {
	ID               string     `json:"id"`
	Managed          string     `json:"managed"`
	Runtime          string     `json:"runtime"`
	RuntimeSessionID string     `json:"runtime_session_id"`
	Status           string     `json:"status"`
	Running          *bool      `json:"running"`
	Paused           *bool      `json:"paused"`
	Restarting       *bool      `json:"restarting"`
	Dead             *bool      `json:"dead"`
	OOMKilled        *bool      `json:"oom_killed"`
	ExitCode         *int       `json:"exit_code"`
	Error            *string    `json:"error"`
	PID              *int       `json:"pid"`
	StartedAt        *time.Time `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
	CreatedAt        time.Time  `json:"created_at"`
	RestartCount     *int       `json:"restart_count"`
	RestartPolicy    string     `json:"restart_policy"`
	FirstCheckedAt   time.Time  `json:"first_checked_at"`
	RecheckedAt      time.Time  `json:"rechecked_at"`
	SnapshotSHA256   string     `json:"snapshot_sha256"`
}

// ValidFullContainerID accepts only Docker's complete lowercase SHA256 IDs.
func ValidFullContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == id
}

func (p RetainedNeverStartedContainer) Valid(runtimeStartedAt, verifiedAt time.Time) bool {
	if !ValidFullContainerID(p.ID) || p.Managed != "true" || p.Runtime != "opencode" || p.Status != "created" || p.Running == nil || *p.Running || p.Paused == nil || *p.Paused || p.Restarting == nil || *p.Restarting || p.Dead == nil || *p.Dead || p.OOMKilled == nil || *p.OOMKilled || p.ExitCode == nil || *p.ExitCode != 0 || p.Error == nil || *p.Error != "" || p.PID == nil || *p.PID != 0 || p.StartedAt == nil || !p.StartedAt.IsZero() || p.FinishedAt == nil || !p.FinishedAt.IsZero() || p.RestartCount == nil || *p.RestartCount != 0 || p.RestartPolicy != "no" || !ValidFullContainerID(p.SnapshotSHA256) {
		return false
	}
	for _, r := range p.RuntimeSessionID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	for _, at := range []time.Time{p.CreatedAt, p.FirstCheckedAt, p.RecheckedAt} {
		_, offset := at.Zone()
		if at.IsZero() || offset != 0 || at.After(verifiedAt) {
			return false
		}
	}
	return !p.CreatedAt.After(p.FirstCheckedAt) && !p.FirstCheckedAt.Before(runtimeStartedAt) && !p.RecheckedAt.Before(p.FirstCheckedAt)
}

// PreexistingCapabilityContainer is an operator-selected historical exclusion,
// not a claim of ownership by the current deployment journal.
type PreexistingCapabilityContainer struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Labels         map[string]string `json:"labels"`
	CreatedAt      time.Time         `json:"created_at"`
	Status         string            `json:"status"`
	Running        *bool             `json:"running"`
	Paused         *bool             `json:"paused"`
	Restarting     *bool             `json:"restarting"`
	Dead           *bool             `json:"dead"`
	PID            *int              `json:"pid"`
	StartedAt      *time.Time        `json:"started_at"`
	FirstCheckedAt time.Time         `json:"first_checked_at"`
	RecheckedAt    time.Time         `json:"rechecked_at"`
	SnapshotSHA256 string            `json:"snapshot_sha256"`
}

func (p PreexistingCapabilityContainer) Valid(runStartedAt, runtimeStartedAt, verifiedAt time.Time) bool {
	if !ValidFullContainerID(p.ID) || p.Labels["io.zoe.managed"] != "true" || p.Name != "/zoe-cap-"+p.Labels["io.zoe.deployment"] || p.Image == "" || p.Status != "running" || p.Running == nil || !*p.Running || p.Paused == nil || *p.Paused || p.Restarting == nil || *p.Restarting || p.Dead == nil || *p.Dead || p.PID == nil || *p.PID < 1 || p.StartedAt == nil || p.StartedAt.IsZero() || p.StartedAt.After(p.FirstCheckedAt) || !ValidFullContainerID(p.SnapshotSHA256) {
		return false
	}
	if _, exists := p.Labels["io.zoe.runtime"]; exists {
		return false
	}
	imageParts := strings.Split(p.Image, "@sha256:")
	if len(imageParts) != 2 || imageParts[0] == "" || !ValidFullContainerID(imageParts[1]) {
		return false
	}
	for _, r := range imageParts[0] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("./:_-", r)) {
			return false
		}
	}
	for _, key := range []string{"io.zoe.workspace", "io.zoe.deployment", "io.zoe.attempt"} {
		value := p.Labels[key]
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 12 || hex.EncodeToString(decoded) != value {
			return false
		}
	}
	if !ValidFullContainerID(p.Labels["io.zoe.release"]) {
		return false
	}
	fence, err := strconv.ParseInt(p.Labels["io.zoe.fence"], 10, 64)
	if err != nil || fence < 1 || strconv.FormatInt(fence, 10) != p.Labels["io.zoe.fence"] {
		return false
	}
	for _, at := range []time.Time{p.CreatedAt, p.FirstCheckedAt, p.RecheckedAt, *p.StartedAt} {
		_, offset := at.Zone()
		if at.IsZero() || offset != 0 || at.After(verifiedAt) {
			return false
		}
	}
	return !runStartedAt.IsZero() && p.CreatedAt.Before(runStartedAt) && !p.CreatedAt.After(*p.StartedAt) && !p.FirstCheckedAt.Before(runtimeStartedAt) && !p.RecheckedAt.Before(p.FirstCheckedAt)
}

// TerminationProof carries the independent runtime-stop evidence required
// before the control plane releases a run's capacity slot.
type TerminationProof struct {
	Source             string    `json:"source"`
	Method             string    `json:"method"`
	RuntimeSessionID   string    `json:"runtime_session_id"`
	RuntimeContainerID string    `json:"runtime_container_id"`
	VerifiedAt         time.Time `json:"verified_at"`
	CleanupComplete    bool      `json:"cleanup_complete"`
}

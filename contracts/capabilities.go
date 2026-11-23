package contracts

import "time"

// CapabilityManifest is immutable once its content digest is admitted. It is
// a constrained service description, not a general container specification.
type CapabilityManifest struct {
	SchemaVersion  int                      `json:"schema_version"`
	ArtifactRef    string                   `json:"artifact_ref"`
	ArtifactDigest string                   `json:"artifact_digest"`
	Entrypoint     string                   `json:"entrypoint"`
	Args           []string                 `json:"args,omitempty"`
	SecretRefs     map[string]string        `json:"secret_refs,omitempty"`
	Mounts         []CapabilityMount        `json:"mounts,omitempty"`
	Capabilities   []string                 `json:"capabilities"`
	HealthCheck    *CapabilityHealthCheck   `json:"health_check,omitempty"`
	Triggers       []CapabilityTrigger      `json:"triggers,omitempty"`
	Budget         CapabilityResourceBudget `json:"budget"`
	Network        CapabilityNetworkPolicy  `json:"network"`
}

type CapabilityMount struct {
	VolumeID string `json:"volume_id"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// CapabilityHealthCheck is executed inside the immutable image. Exec probes
// use an argv vector and a bare executable name; shell text and host paths
// are not accepted.
type CapabilityHealthCheck struct {
	Kind             string   `json:"kind"`
	Target           string   `json:"target,omitempty"`
	Port             int      `json:"port,omitempty"`
	Args             []string `json:"args,omitempty"`
	IntervalSeconds  int      `json:"interval_seconds"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
	FailureThreshold int      `json:"failure_threshold"`
}

type CapabilityTrigger struct {
	Kind            string                    `json:"kind"`
	Source          string                    `json:"source"`
	Mode            string                    `json:"mode"`
	Scopes          []string                  `json:"scopes,omitempty"`
	IntervalSeconds int64                     `json:"interval_seconds,omitempty"`
	MissedRunPolicy string                    `json:"missed_run_policy,omitempty"`
	Filters         []CapabilityTriggerFilter `json:"filters,omitempty"`
	FilterJSON      string                    `json:"filter_json,omitempty"` // rejected; use typed Filters
}

type CapabilityTriggerFilter struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    string `json:"value,omitempty"`
}

type CapabilityResourceBudget struct {
	CPUMillis      int64 `json:"cpu_millis"`
	MemoryBytes    int64 `json:"memory_bytes"`
	DiskBytes      int64 `json:"disk_bytes"`
	TimeoutSeconds int64 `json:"timeout_seconds"`
	Concurrency    int   `json:"concurrency"`
}

type CapabilityNetworkPolicy struct {
	Mode         string   `json:"mode"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

type CapabilityRelease struct {
	ReleaseID string             `json:"release_id"`
	Digest    string             `json:"digest"`
	Manifest  CapabilityManifest `json:"manifest"`
	CreatedAt time.Time          `json:"created_at"`
}

type CapabilityGrant struct {
	GrantID     string                   `json:"grant_id"`
	ScopeKind   string                   `json:"scope_kind"`
	ScopeID     string                   `json:"scope_id,omitempty"`
	PrincipalID string                   `json:"principal_id,omitempty"`
	Actions     []string                 `json:"actions"`
	Budget      CapabilityResourceBudget `json:"budget"`
	ExpiresAt   time.Time                `json:"expires_at"`
}

type DeploymentClaim struct {
	InstanceLineageID string             `json:"instance_lineage_id"`
	DeploymentID      string             `json:"deployment_id"`
	AttemptID         string             `json:"attempt_id"`
	Fence             int64              `json:"fence"`
	ServiceID         string             `json:"service_id"`
	ReleaseDigest     string             `json:"release_digest"`
	Manifest          CapabilityManifest `json:"manifest"`
	DesiredOp         string             `json:"desired_operation"`
	Grant             CapabilityGrant    `json:"grant"`
	ConfigRef         string             `json:"config_ref,omitempty"`
	ConfigSHA256      string             `json:"config_sha256,omitempty"`
	VolumeIDs         []string           `json:"volume_ids,omitempty"`
	RuntimeBackend    string             `json:"runtime_backend"`
	ClaimState        string             `json:"claim_state,omitempty"`
	DesiredControl    string             `json:"desired_control,omitempty"`
	CleanupOnly       bool               `json:"cleanup_only,omitempty"`
}

type DeploymentObservation struct {
	DeploymentID      string                      `json:"deployment_id"`
	AttemptID         string                      `json:"attempt_id"`
	Fence             int64                       `json:"fence"`
	ObservationID     string                      `json:"observation_id"`
	Operation         string                      `json:"operation"`
	State             string                      `json:"state"`
	ExternalEffectRef []string                    `json:"external_effect_refs,omitempty"`
	Health            CapabilityHealth            `json:"health"`
	TerminationProof  *CapabilityTerminationProof `json:"termination_proof,omitempty"`
	ErrorCode         string                      `json:"error_code,omitempty"`
	ObservedAt        time.Time                   `json:"observed_at"`
}

// CapabilityTriggerClaim is an immutable, authorized program invocation.
// Worker/Super admission is owned by the control-plane task source path.
type CapabilityTriggerClaim struct {
	DeploymentID  string             `json:"deployment_id"`
	ServiceID     string             `json:"service_id"`
	TriggerID     string             `json:"trigger_id"`
	TriggerRunID  string             `json:"trigger_run_id"`
	TriggerKey    string             `json:"trigger_key"`
	EventID       string             `json:"event_id,omitempty"`
	ScheduledFor  time.Time          `json:"scheduled_for,omitempty"`
	AttemptID     string             `json:"attempt_id"`
	Fence         int64              `json:"fence"`
	ReleaseDigest string             `json:"release_digest"`
	Manifest      CapabilityManifest `json:"manifest"`
	Mode          string             `json:"mode"`
	Scopes        []string           `json:"scopes,omitempty"`
	Grant         CapabilityGrant    `json:"grant"`
	ConfigRef     string             `json:"config_ref,omitempty"`
	ConfigSHA256  string             `json:"config_sha256,omitempty"`
	EventPayload  []byte             `json:"event_payload,omitempty"`
	ScopeKind     string             `json:"scope_kind"`
	ScopeID       string             `json:"scope_id,omitempty"`
}

type CapabilityTriggerObservation struct {
	TriggerRunID      string    `json:"trigger_run_id"`
	AttemptID         string    `json:"attempt_id"`
	Fence             int64     `json:"fence"`
	ObservationID     string    `json:"observation_id"`
	State             string    `json:"state"`
	TaskID            string    `json:"task_id,omitempty"`
	RunID             string    `json:"run_id,omitempty"`
	ExternalEffectRef []string  `json:"external_effect_refs,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ObservedAt        time.Time `json:"observed_at"`
}

type CapabilityHealth struct {
	State       string    `json:"state"`
	CheckedAt   time.Time `json:"checked_at,omitempty"`
	EvidenceRef string    `json:"evidence_ref,omitempty"`
}

type CapabilityTerminationProof struct {
	EffectRef       string    `json:"effect_ref"`
	TerminatedAt    time.Time `json:"terminated_at"`
	CleanupComplete bool      `json:"cleanup_complete"`
}

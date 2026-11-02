package contracts

// PermissionRule is the OpenCode session permission contract.
type PermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

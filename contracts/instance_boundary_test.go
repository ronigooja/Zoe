package contracts

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestProductContractsHaveNoWorkspaceIdentity(t *testing.T) {
	for _, value := range []any{Principal{}, Event{}, Snapshot{}, JobClaim{}, DeploymentClaim{}, CapabilityTriggerClaim{}} {
		typeOf := reflect.TypeOf(value)
		for i := 0; i < typeOf.NumField(); i++ {
			field := typeOf.Field(i)
			if strings.Contains(strings.ToLower(field.Name), "workspace") {
				t.Fatalf("%s retains product tenancy field %s", typeOf.Name(), field.Name)
			}
		}
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "workspace_id") {
			t.Fatalf("%s retains product tenancy wire field", typeOf.Name())
		}
	}
}

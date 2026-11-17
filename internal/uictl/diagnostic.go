package uictl

import "errors"

// validationError contains validator-owned identifiers only, never input values.
type validationError struct {
	class string
	field string
}

func (e *validationError) Error() string { return e.class }

func validationField(class, field string) error {
	return &validationError{class: class, field: field}
}

// FailureCode applies to parser failures persisted or logged by UI jobs. Untyped
// decoder errors are collapsed because their messages can contain input content.
func FailureCode(err error) string {
	var failure *validationError
	if !errors.As(err, &failure) {
		// Only exact validator-owned classes pass through; decoder text does not.
		switch err.Error() {
		case "duplicate_field", "invalid_object", "invalid_array", "invalid_json",
			"trailing_data", "object_required", "unknown_field", "output_limit_or_utf8",
			"size_invalid", "bool_required", "container_invalid", "input_semantics",
			"feedback_semantics", "message_limit", "mixed_surfaces", "message_kind",
			"surface_id", "catalog", "send_data_model", "theme", "components", "component":
			return err.Error()
		}
		return "invalid_json"
	}
	if failure.field != "" {
		return failure.class + ":" + failure.field
	}
	return failure.class
}

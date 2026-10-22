package qjsbox

import "encoding/json"

// HelperTest is extracted by the bounded structural parser from generated
// three-valued code. It lets publication validate the same literal choices as
// visual conditions, without executing user code or trusting submitted metadata.
type HelperTest struct {
	Reference string          `json:"reference"`
	Operator  string          `json:"operator"`
	Operand   json.RawMessage `json:"operand,omitempty"`
}

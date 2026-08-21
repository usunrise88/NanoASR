package openai

import (
	"encoding/json"
	"net/http"

	"github.com/usunrise88/nanoasr/internal/core"
)

// errorEnvelope is the OpenAI error shape. Clients parse this strictly, so the
// field names are part of the contract.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param,omitempty"`
		Code    string `json:"code"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, err error) {
	e := core.AsError(err)
	var env errorEnvelope
	env.Error.Message = e.Message
	env.Error.Type = openAIType(e.Code)
	env.Error.Param = e.Param
	env.Error.Code = string(e.Code)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(env)
}

// openAIType maps our codes onto the small vocabulary OpenAI clients branch on.
func openAIType(c core.Code) string {
	switch c {
	case core.CodeInvalidRequest, core.CodeCapabilityUnavailable,
		core.CodeFileTooLarge, core.CodeDurationExceeded, core.CodeUnsupportedMediaType:
		return "invalid_request_error"
	case core.CodeUnauthorized, core.CodeModelForbidden:
		return "authentication_error"
	case core.CodeQueueFull, core.CodeRateLimited:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}

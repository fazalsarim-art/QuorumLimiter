package api

import (
	"encoding/json"
	"net/http"
)

// APIError is the stable error envelope returned by all endpoints.
type APIError struct {
	Error APIErrorBody `json:"error"`
}

// APIErrorBody carries a stable machine code, a user-safe message, and the
// request id for correlation.
type APIErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, code, message, requestID string) {
	writeJSON(w, status, APIError{Error: APIErrorBody{Code: code, Message: message, RequestID: requestID}})
}

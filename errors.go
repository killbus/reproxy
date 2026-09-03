package reproxy

import (
	"encoding/json"
	"net/http"
)

// RequestError is the shared error contract for client-visible request
// failures. It carries enough context to render a structured error response
// that names the specific reason (and, where useful, how to fix it).
type RequestError struct {
	// Code is the HTTP status code to respond with.
	Code int
	// Reason names the specific failure, suitable for direct display.
	Reason string
	// Hint offers corrective guidance for the client.
	Hint string
}

// Error implements the error interface.
func (e *RequestError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Reason
}

// Write renders the error as a JSON body: {"error": ..., "hint": ...}.
func (e *RequestError) Write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(e.Code)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
		Hint  string `json:"hint,omitempty"`
	}{Error: e.Reason, Hint: e.Hint})
}

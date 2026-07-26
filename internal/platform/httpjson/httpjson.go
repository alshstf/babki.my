// Package httpjson provides small helpers for JSON request/response handling
// shared by all domain modules.
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Write encodes v as JSON with the given status code.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error writes a JSON error body {"error": msg}.
func Error(w http.ResponseWriter, status int, msg string) {
	Write(w, status, map[string]string{"error": msg})
}

// Decode reads the request body into dst (max 1MB, unknown fields rejected).
// On failure it writes a 400 response and returns the error.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			Error(w, http.StatusRequestEntityTooLarge, "request body too large")
			return err
		}
		Error(w, http.StatusBadRequest, fmt.Sprintf("invalid json: %v", err))
		return err
	}
	return nil
}

package otlp

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
)

// Default limits: an export is a few hundred KB at most.
const (
	defaultMaxBody    = 8 << 20  // bytes on the wire
	defaultMaxDecoded = 32 << 20 // bytes after gunzip (gzip bomb guard)
)

// Receiver is the agent's OTLP/HTTP endpoint, POST /v1/traces, JSON encoding
// only (D-010), optionally gzip-compressed. Each parsed export goes to Sink.
type Receiver struct {
	Mapping    *Mapping
	Sink       func(Result)
	MaxBody    int64 // 0 = defaultMaxBody
	MaxDecoded int64 // 0 = defaultMaxDecoded
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "only OTLP/HTTP with JSON encoding is accepted; set the exporter's encoding to json")
		return
	}
	maxBody, maxDecoded := r.MaxBody, r.MaxDecoded
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	if maxDecoded <= 0 {
		maxDecoded = defaultMaxDecoded
	}
	var body io.Reader = http.MaxBytesReader(w, req.Body, maxBody)
	switch req.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			if isTooLarge(err) {
				fail(w, http.StatusRequestEntityTooLarge, errTooLarge.Error())
				return
			}
			fail(w, http.StatusBadRequest, "body is not valid gzip")
			return
		}
		defer zr.Close()
		body = zr
	default:
		fail(w, http.StatusUnsupportedMediaType, "unsupported Content-Encoding; use gzip or none")
		return
	}
	data, err := io.ReadAll(io.LimitReader(body, maxDecoded+1))
	switch {
	case isTooLarge(err) || int64(len(data)) > maxDecoded:
		fail(w, http.StatusRequestEntityTooLarge, errTooLarge.Error())
		return
	case err != nil:
		fail(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	res, err := Parse(data, r.Mapping)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error()) // Parse errors never quote the body
		return
	}
	r.Sink(res)
	w.Header().Set("Content-Type", "application/json")
	if res.Rejected == 0 {
		_, _ = io.WriteString(w, "{}\n")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"partialSuccess": map[string]string{
		"rejectedSpans": strconv.Itoa(res.Rejected),
		"errorMessage":  fmt.Sprintf("%d span(s) without a valid trace ID or start time were dropped", res.Rejected),
	}})
}

func isTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// fail writes an OTLP-style error status. Messages are fixed text: request
// content is never echoed back.
func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}

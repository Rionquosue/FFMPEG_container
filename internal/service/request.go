package service

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
)

type Request struct {
	Operation string `json:"operation"`
	Input     struct {
		Size   int64  `json:"size_bytes"`
		SHA256 string `json:"sha256"`
	} `json:"input"`
	Parameters   map[string]json.RawMessage `json:"parameters"`
	Format       string                     `json:"output_format"`
	declaredMIME string
}
type Params struct {
	Width, Height, Quality, Bitrate, Channels, JPEGQuality int
	MaxDimension, MaxFPS, MaxBitrate                       int
	Fit, AudioMode                                         string
	Timestamp, Start, Duration                             float64
}

// operationParameters lists the parameters each operation accepts. Validate
// and the capabilities response both read it, so they cannot drift apart.
var operationParameters = map[string]string{
	"transcode":     "width height fit quality audio_bitrate_kbps audio_mode max_dimension max_fps max_bitrate_kbps",
	"resize":        "width height fit jpeg_quality",
	"thumbnail":     "timestamp_seconds width height fit jpeg_quality",
	"extract_audio": "audio_bitrate_kbps channels",
	"trim":          "start_seconds duration_seconds quality width height fit audio_bitrate_kbps audio_mode max_dimension max_fps max_bitrate_kbps",
	"remux":         "audio_mode",
	"metadata":      "",
}

// OperationParameters returns the accepted parameter names per operation.
func OperationParameters() map[string][]string {
	out := map[string][]string{}
	for op, names := range operationParameters {
		out[op] = append([]string{}, splitFields(names)...)
	}
	return out
}

func decodeStrict(r io.Reader, v any, max int64) error {
	b, e := io.ReadAll(io.LimitReader(r, max+1))
	if e != nil {
		return e
	}
	if int64(len(b)) > max {
		return errors.New("JSON too large")
	}
	// Duplicate object keys are rejected recursively, including escaped duplicates.
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					t, e = d.Token()
					if e != nil {
						return e
					}
					k, ok := t.(string)
					if !ok || seen[k] {
						return errors.New("duplicate key")
					}
					seen[k] = true
					if e = walk(); e != nil {
						return e
					}
				}
			case '[':
				for d.More() {
					if e = walk(); e != nil {
						return e
					}
				}
			default:
				return errors.New("invalid JSON")
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	if e = walk(); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// invalid builds a synchronous 422 error (03, Synchronous HTTP errors).
func invalid(code, field, message string) *APIError {
	return &APIError{Code: code, Field: field, Message: message}
}

// Validate checks the request part before any file bytes are read. Errors
// carry the contract's code and the offending field.
func (r Request) Validate(c Config) (Params, *APIError) {
	p := Params{Quality: 23, Bitrate: 128, Channels: 2, Fit: "contain", JPEGQuality: 85, AudioMode: "preserve"}
	hash, e := hex.DecodeString(r.Input.SHA256)
	if e != nil || len(hash) != 32 {
		return p, invalid("invalid_parameter", "input.sha256", "SHA-256 must be 64 hexadecimal characters.")
	}
	if r.Input.Size < 1 || r.Input.Size > c.MaxUpload {
		return p, invalid("invalid_parameter", "input.size_bytes", "Input size must be between 1 byte and the advertised maximum.")
	}
	allowed, ok := operationParameters[r.Operation]
	if !ok {
		return p, invalid("unsupported_operation", "operation", "Unknown operation.")
	}
	formats := map[string][]string{"transcode": {"mp4", "webm"}, "resize": {"jpg", "png"}, "thumbnail": {"jpg", "png"}, "extract_audio": {"m4a", "mp3", "opus"}, "trim": {"mp4", "webm"}, "remux": {"mp4", "webm"}, "metadata": {"json"}}
	if !contains(formats[r.Operation], r.Format) {
		return p, invalid("incompatible_output", "output_format", "This output format is not available for the operation.")
	}
	if r.Parameters == nil {
		return p, invalid("invalid_parameter", "parameters", "Parameters must be an object.")
	}
	set := map[string]bool{}
	for _, f := range splitFields(allowed) {
		set[f] = true
	}
	for k, v := range r.Parameters {
		field := "parameters." + k
		if !set[k] {
			return p, invalid("invalid_parameter", field, "Unknown parameter for this operation.")
		}
		if bytes.Equal(v, []byte("null")) {
			return p, invalid("invalid_parameter", field, "Parameter must not be null.")
		}
		var dest any
		switch k {
		case "width":
			dest = &p.Width
		case "height":
			dest = &p.Height
		case "quality":
			dest = &p.Quality
		case "audio_bitrate_kbps":
			dest = &p.Bitrate
		case "channels":
			dest = &p.Channels
		case "fit":
			dest = &p.Fit
		case "jpeg_quality":
			dest = &p.JPEGQuality
		case "timestamp_seconds":
			dest = &p.Timestamp
		case "start_seconds":
			dest = &p.Start
		case "duration_seconds":
			dest = &p.Duration
		case "audio_mode":
			dest = &p.AudioMode
		case "max_dimension":
			dest = &p.MaxDimension
		case "max_fps":
			dest = &p.MaxFPS
		case "max_bitrate_kbps":
			dest = &p.MaxBitrate
		}
		if json.Unmarshal(v, dest) != nil {
			return p, invalid("invalid_parameter", field, "Parameter has the wrong type.")
		}
	}
	switch {
	case p.Quality < 18 || p.Quality > 35:
		return p, invalid("invalid_parameter", "parameters.quality", "Quality must be an integer between 18 and 35.")
	case p.Bitrate < 64 || p.Bitrate > 320:
		return p, invalid("invalid_parameter", "parameters.audio_bitrate_kbps", "Audio bitrate must be between 64 and 320 kbps.")
	case p.Channels < 1 || p.Channels > 2:
		return p, invalid("invalid_parameter", "parameters.channels", "Channels must be 1 or 2.")
	case !contains([]string{"contain", "cover"}, p.Fit):
		return p, invalid("invalid_parameter", "parameters.fit", "Fit must be contain or cover.")
	case p.JPEGQuality < 50 || p.JPEGQuality > 95:
		return p, invalid("invalid_parameter", "parameters.jpeg_quality", "JPEG quality must be an integer between 50 and 95.")
	case !contains([]string{"preserve", "remove"}, p.AudioMode):
		return p, invalid("invalid_parameter", "parameters.audio_mode", "Audio mode must be preserve or remove.")
	}
	if _, ok := r.Parameters["max_dimension"]; ok && (p.MaxDimension < 16 || p.MaxDimension > 4096 || p.MaxDimension%2 != 0) {
		return p, invalid("invalid_parameter", "parameters.max_dimension", "Maximum dimension must be an even integer between 16 and 4096.")
	}
	if _, ok := r.Parameters["max_fps"]; ok && (p.MaxFPS < 1 || p.MaxFPS > 60) {
		return p, invalid("invalid_parameter", "parameters.max_fps", "Maximum frame rate must be an integer between 1 and 60.")
	}
	if _, ok := r.Parameters["max_bitrate_kbps"]; ok {
		if p.MaxBitrate < 100 || p.MaxBitrate > 20000 {
			return p, invalid("invalid_parameter", "parameters.max_bitrate_kbps", "Maximum bitrate must be an integer between 100 and 20000 kbps.")
		}
		if r.Format != "mp4" {
			return p, invalid("invalid_parameter", "parameters.max_bitrate_kbps", "Maximum bitrate applies only to mp4 output.")
		}
	}
	if _, ok := r.Parameters["jpeg_quality"]; ok && r.Format != "jpg" {
		return p, invalid("invalid_parameter", "parameters.jpeg_quality", "JPEG quality applies only to jpg output.")
	}
	_, wset := r.Parameters["width"]
	_, hset := r.Parameters["height"]
	if _, ok := r.Parameters["max_dimension"]; ok && (wset || hset) {
		return p, invalid("invalid_parameter", "parameters.max_dimension", "Maximum dimension cannot be combined with width and height.")
	}
	if wset != hset {
		missing := "parameters.height"
		if hset {
			missing = "parameters.width"
		}
		return p, invalid("invalid_parameter", missing, "Width and height must be given together.")
	}
	if (r.Operation == "resize" || r.Operation == "thumbnail") && !wset {
		return p, invalid("invalid_parameter", "parameters.width", "Width and height are required.")
	}
	if wset {
		for _, d := range []struct {
			name string
			v    int
		}{{"width", p.Width}, {"height", p.Height}} {
			if d.v < 16 || d.v > 4096 || ((r.Operation == "transcode" || r.Operation == "trim") && d.v%2 != 0) {
				return p, invalid("invalid_parameter", "parameters."+d.name, "Dimensions must be integers between 16 and 4096, even for video.")
			}
		}
		if int64(p.Width)*int64(p.Height) > c.MaxPixels {
			return p, invalid("invalid_parameter", "parameters.width", "Width × height exceeds the pixel limit.")
		}
	}
	for _, t := range []struct {
		name string
		v    float64
	}{{"start_seconds", p.Start}, {"duration_seconds", p.Duration}, {"timestamp_seconds", p.Timestamp}} {
		if math.IsNaN(t.v) || math.IsInf(t.v, 0) || t.v < 0 || t.v > c.MaxDuration {
			return p, invalid("invalid_parameter", "parameters."+t.name, "Time must be a finite number within the duration limit.")
		}
	}
	if r.Operation == "thumbnail" {
		if _, ok := r.Parameters["timestamp_seconds"]; !ok {
			return p, invalid("invalid_parameter", "parameters.timestamp_seconds", "Timestamp is required.")
		}
	}
	if r.Operation == "trim" {
		if _, ok := r.Parameters["start_seconds"]; !ok {
			return p, invalid("invalid_parameter", "parameters.start_seconds", "Start is required.")
		}
		if p.Duration <= 0 {
			return p, invalid("invalid_parameter", "parameters.duration_seconds", "Duration must be greater than zero.")
		}
	}
	return p, nil
}
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
func splitFields(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

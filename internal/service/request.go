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
	Width, Height, Quality, Bitrate, Channels int
	Fit                                       string
	Timestamp, Start, Duration                float64
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

func (r Request) Validate(c Config) (Params, error) {
	p := Params{Quality: 23, Bitrate: 128, Channels: 2, Fit: "contain"}
	bad := errors.New("unsupported operation, format or parameters")
	if r.Parameters == nil {
		return p, bad
	}
	hash, e := hex.DecodeString(r.Input.SHA256)
	if e != nil || len(hash) != 32 || r.Input.Size < 1 || r.Input.Size > c.MaxUpload {
		return p, errors.New("invalid input size or checksum")
	}
	fields := map[string]string{
		"transcode": "width height fit quality audio_bitrate_kbps",
		"resize":    "width height fit", "thumbnail": "timestamp_seconds width height fit",
		"extract_audio": "audio_bitrate_kbps channels", "trim": "start_seconds duration_seconds quality",
		"remux": "", "metadata": "",
	}
	allowed, ok := fields[r.Operation]
	if !ok {
		return p, bad
	}
	set := map[string]bool{}
	for _, f := range splitFields(allowed) {
		set[f] = true
	}
	for k, v := range r.Parameters {
		if !set[k] || bytes.Equal(v, []byte("null")) {
			return p, bad
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
		case "timestamp_seconds":
			dest = &p.Timestamp
		case "start_seconds":
			dest = &p.Start
		case "duration_seconds":
			dest = &p.Duration
		}
		if json.Unmarshal(v, dest) != nil {
			return p, bad
		}
	}
	formats := map[string][]string{"transcode": {"mp4", "webm"}, "resize": {"jpg", "png"}, "thumbnail": {"jpg", "png"}, "extract_audio": {"m4a", "mp3", "opus"}, "trim": {"mp4", "webm"}, "remux": {"mp4", "webm"}, "metadata": {"json"}}
	if !contains(formats[r.Operation], r.Format) {
		return p, bad
	}
	if p.Quality < 18 || p.Quality > 35 || p.Bitrate < 64 || p.Bitrate > 320 || p.Channels < 1 || p.Channels > 2 || !contains([]string{"contain", "cover"}, p.Fit) {
		return p, bad
	}
	_, wset := r.Parameters["width"]
	_, hset := r.Parameters["height"]
	if wset != hset {
		return p, bad
	}
	if r.Operation == "resize" || r.Operation == "thumbnail" {
		if !wset {
			return p, bad
		}
	}
	if wset && (p.Width < 16 || p.Height < 16 || p.Width > 4096 || p.Height > 4096 || int64(p.Width)*int64(p.Height) > c.MaxPixels) {
		return p, bad
	}
	if r.Operation == "transcode" && (p.Width%2 != 0 || p.Height%2 != 0) {
		return p, bad
	}
	for _, v := range []float64{p.Start, p.Duration, p.Timestamp} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > c.MaxDuration {
			return p, bad
		}
	}
	if r.Operation == "thumbnail" {
		if _, ok := r.Parameters["timestamp_seconds"]; !ok {
			return p, bad
		}
	}
	if r.Operation == "trim" {
		if _, ok := r.Parameters["start_seconds"]; !ok || p.Duration <= 0 {
			return p, bad
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

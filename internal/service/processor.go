package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const inputFormats = "mov,matroska,mp3,wav,flac,ogg,aac,png_pipe,jpeg_pipe"

type Execute func(context.Context, string, string, []string) ([]byte, error)
type MediaProcessor struct {
	Config  Config
	Execute Execute
}

// Probe failures that map to their own asynchronous error codes (03).
var (
	errDuration   = errors.New("duration limit")
	errDimensions = errors.New("pixel limit")
)

type probeData struct {
	Streams []struct {
		Type     string `json:"codec_type"`
		Codec    string `json:"codec_name"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Channels int    `json:"channels"`
		SideData []struct {
			Rotation int `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
	} `json:"format"`
}
type metadata struct {
	Format     string  `json:"format"`
	Duration   float64 `json:"duration_seconds"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	VideoCodec string  `json:"video_codec,omitempty"`
	AudioCodec string  `json:"audio_codec,omitempty"`
	Channels   int     `json:"channels,omitempty"`
	Image      bool    `json:"still_image"`
}

func limitedArgs(file string) []string {
	return []string{"-v", "error", "-max_alloc", "67108864", "-threads", "1", "-protocol_whitelist", "file,pipe", "-format_whitelist", inputFormats, "-probesize", "5000000", "-analyzeduration", "5000000", "-i", file}
}
func (p *MediaProcessor) probe(ctx context.Context, dir, file string) (metadata, error) {
	var out metadata
	args := append(limitedArgs(file), "-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height,channels:stream_side_data=rotation", "-of", "json")
	data, e := p.Execute(ctx, dir, "ffprobe", args)
	if e != nil {
		return out, e
	}
	var raw probeData
	if json.Unmarshal(data, &raw) != nil || len(raw.Streams) == 0 || len(raw.Streams) > 16 {
		return out, errors.New("invalid probe")
	}
	out.Format = raw.Format.Name
	if raw.Format.Duration != "" && raw.Format.Duration != "N/A" {
		out.Duration, e = strconv.ParseFloat(raw.Format.Duration, 64)
		if e != nil || math.IsInf(out.Duration, 0) || math.IsNaN(out.Duration) || out.Duration < 0 || out.Duration > p.Config.MaxDuration {
			return out, errDuration
		}
	}
	for _, s := range raw.Streams {
		if s.Type == "video" {
			if s.Width < 1 || s.Height < 1 || int64(s.Width)*int64(s.Height) > p.Config.MaxPixels {
				return out, errDimensions
			}
			if out.VideoCodec == "" {
				out.VideoCodec = s.Codec
				out.Width = s.Width
				out.Height = s.Height
				for _, sd := range s.SideData {
					if sd.Rotation%180 != 0 {
						out.Width, out.Height = out.Height, out.Width
					}
				}
			}
		}
		if s.Type == "audio" {
			if s.Channels < 1 || s.Channels > 8 {
				return out, errors.New("channel limit")
			}
			if out.AudioCodec == "" {
				out.AudioCodec = s.Codec
				out.Channels = s.Channels
			}
		}
	}
	out.Image = raw.Format.Name == "png_pipe" || raw.Format.Name == "jpeg_pipe"
	if !out.Image && out.Duration <= 0 {
		return out, errors.New("unknown duration")
	}
	return out, nil
}

// Asynchronous error codes the contract marks as retryable (03, Asynchronous job errors).
var retryableFaults = map[string]bool{"processing_timeout": true, "resource_limit": true, "processing_failed": true}

func fault(code string) *APIError {
	return &APIError{Code: code, Message: strings.ReplaceAll(code, "_", " ") + ".", Retryable: retryableFaults[code]}
}
func faultAt(code, field string) *APIError {
	f := fault(code)
	f.Field = field
	return f
}
func probeFault(e error) *APIError {
	switch {
	case errors.Is(e, errDuration):
		return fault("duration_exceeded")
	case errors.Is(e, errDimensions):
		return fault("dimensions_exceeded")
	}
	return fault("invalid_media")
}
func (p *MediaProcessor) Process(ctx context.Context, dir string, r Request, v Params) (*Result, *APIError) {
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "output")
	f, e := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, fault("processing_failed")
	}
	f.Close()
	m, e := p.probe(ctx, dir, input)
	if e != nil {
		if ctx.Err() != nil {
			return nil, fault("processing_timeout")
		}
		return nil, probeFault(e)
	}
	if claimed := r.declaredMIME; claimed != "" && claimed != "application/octet-stream" {
		allowed := map[string][]string{
			"mov,mp4,m4a,3gp,3g2,mj2": {"video/mp4", "video/quicktime", "audio/mp4"},
			"matroska,webm":           {"video/webm", "audio/webm", "video/x-matroska"},
			"png_pipe":                {"image/png"}, "jpeg_pipe": {"image/jpeg"},
			"mp3": {"audio/mpeg"}, "wav": {"audio/wav", "audio/x-wav"},
			"flac": {"audio/flac", "audio/x-flac"}, "ogg": {"audio/ogg", "video/ogg", "application/ogg"}, "aac": {"audio/aac"},
		}
		if !contains(allowed[m.Format], claimed) {
			// The declared type does not match the detected format.
			return nil, fault("unsupported_media")
		}
	}
	if v.Width > 0 {
		scale := math.Min(float64(v.Width)/float64(m.Width), float64(v.Height)/float64(m.Height))
		if v.Fit == "cover" {
			scale = math.Max(float64(v.Width)/float64(m.Width), float64(v.Height)/float64(m.Height))
		}
		if m.Width == 0 || scale > 1 {
			return nil, faultAt("upscale_not_allowed", "parameters.width")
		}
	}
	if r.Operation == "resize" && !m.Image {
		return nil, fault("unsupported_media")
	}
	if contains([]string{"transcode", "trim", "thumbnail", "remux"}, r.Operation) && (m.Image || m.VideoCodec == "") {
		return nil, fault("unsupported_media")
	}
	if r.Operation == "thumbnail" && v.Timestamp >= m.Duration {
		return nil, faultAt("range_out_of_bounds", "parameters.timestamp_seconds")
	}
	if r.Operation == "trim" && v.Start >= m.Duration {
		return nil, faultAt("range_out_of_bounds", "parameters.start_seconds")
	}
	if r.Operation == "trim" && v.Start+v.Duration > m.Duration {
		return nil, faultAt("range_out_of_bounds", "parameters.duration_seconds")
	}
	if r.Operation == "extract_audio" && m.AudioCodec == "" {
		return nil, fault("no_audio_track")
	}
	if r.Operation == "remux" {
		compatible := r.Format == "mp4" && m.VideoCodec == "h264" && (m.AudioCodec == "" || m.AudioCodec == "aac") || r.Format == "webm" && contains([]string{"vp8", "vp9", "av1"}, m.VideoCodec) && (m.AudioCodec == "" || contains([]string{"opus", "vorbis"}, m.AudioCodec))
		if !compatible {
			return nil, faultAt("incompatible_output", "output_format")
		}
	}
	if r.Operation == "metadata" {
		data, _ := json.Marshal(m)
		if int64(len(data)) > p.Config.MaxOutput {
			return nil, fault("output_too_large")
		}
		e = os.WriteFile(output, append(data, '\n'), 0600)
	} else {
		args := buildArgs(input, output, r, v, m, p.Config)
		_, e = p.Execute(ctx, dir, "ffmpeg", args)
	}
	if ctx.Err() != nil {
		return nil, fault("processing_timeout")
	}
	info, statErr := os.Stat(output)
	if statErr == nil && info.Size() >= p.Config.MaxOutput {
		return nil, fault("output_too_large")
	}
	if e != nil || statErr != nil || info.Size() == 0 {
		return nil, fault("processing_failed")
	}
	if r.Operation != "metadata" {
		if _, e = p.probe(ctx, dir, output); e != nil {
			if ctx.Err() != nil {
				return nil, fault("processing_timeout")
			}
			return nil, fault("processing_failed")
		}
	}
	stream, e := os.Open(output)
	if e != nil {
		return nil, fault("processing_failed")
	}
	defer stream.Close()
	h := sha256.New()
	n, e := io.Copy(h, stream)
	if e != nil {
		return nil, fault("processing_failed")
	}
	types := map[string]string{"mp4": "video/mp4", "webm": "video/webm", "jpg": "image/jpeg", "png": "image/png", "m4a": "audio/mp4", "mp3": "audio/mpeg", "opus": "audio/ogg", "json": "application/json"}
	return &Result{Type: types[r.Format], Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
func buildArgs(input, output string, r Request, p Params, m metadata, c Config) []string {
	args := []string{"-hide_banner", "-nostdin", "-y"}
	args = append(args, limitedArgs(input)...)
	args = append(args, "-threads", "1", "-filter_threads", "1", "-filter_complex_threads", "1", "-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn")
	fnum := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	if r.Operation == "thumbnail" {
		args = append(args, "-ss", fnum(p.Timestamp))
	}
	if r.Operation == "trim" {
		args = append(args, "-ss", fnum(p.Start), "-t", fnum(p.Duration))
	}
	if p.Width > 0 {
		fit := "decrease"
		if p.Fit == "cover" {
			fit = "increase"
		}
		filter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=%s", p.Width, p.Height, fit)
		if r.Format == "mp4" || r.Format == "webm" {
			filter += ":force_divisible_by=2"
		}
		if p.Fit == "contain" {
			filter += fmt.Sprintf(",pad=%d:%d:(ow-iw)/2:(oh-ih)/2", p.Width, p.Height)
		} else {
			filter += fmt.Sprintf(",crop=%d:%d", p.Width, p.Height)
		}
		args = append(args, "-vf", filter+",setsar=1")
	}
	switch r.Operation {
	case "resize", "thumbnail":
		args = append(args, "-map", "0:v:0", "-frames:v", "1", "-an")
		if r.Format == "jpg" {
			args = append(args, "-c:v", "mjpeg", "-q:v", strconv.Itoa(jpegQScale(p.JPEGQuality)))
		} else {
			args = append(args, "-c:v", "png")
		}
	case "extract_audio":
		args = append(args, "-map", "0:a:0", "-vn", "-ac", strconv.Itoa(p.Channels))
		codec := map[string]string{"mp3": "libmp3lame", "m4a": "aac", "opus": "libopus"}[r.Format]
		args = append(args, "-c:a", codec, "-b:a", strconv.Itoa(p.Bitrate)+"k")
	case "remux":
		args = append(args, "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy")
	default:
		args = append(args, "-map", "0:v:0", "-map", "0:a:0?")
		if r.Format == "mp4" {
			args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-crf", strconv.Itoa(p.Quality), "-c:a", "aac")
		} else {
			args = append(args, "-c:v", "libvpx-vp9", "-b:v", "0", "-crf", strconv.Itoa(p.Quality), "-c:a", "libopus")
		}
		args = append(args, "-b:a", strconv.Itoa(p.Bitrate)+"k")
	}
	mux := map[string]string{"mp4": "mp4", "webm": "webm", "jpg": "image2", "png": "image2", "m4a": "ipod", "mp3": "mp3", "opus": "ogg"}[r.Format]
	if r.Format == "mp4" || r.Format == "m4a" {
		args = append(args, "-movflags", "+faststart")
	}
	if r.Format == "jpg" || r.Format == "png" {
		args = append(args, "-update", "1")
	}
	if r.Operation != "trim" {
		args = append(args, "-t", fnum(c.MaxDuration))
	}
	return append(args, "-f", mux, output)
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(v []byte) (int, error) {
	n := len(v)
	if b.Len()+n > b.limit {
		remain := b.limit - b.Len()
		if remain > 0 {
			b.Buffer.Write(v[:remain])
		}
		return n, nil
	}
	b.Buffer.Write(v)
	return n, nil
}
func RunProcess(ctx context.Context, executable string, args []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "OMP_NUM_THREADS=1", "OPENBLAS_NUM_THREADS=1", "HOME=/nonexistent"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if e == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return e
	}
	cmd.WaitDelay = 2 * time.Second
	stdout := &cappedBuffer{limit: 1 << 20}
	stderr := &cappedBuffer{limit: 65536}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	e := cmd.Run()
	if stdout.Len() >= stdout.limit {
		return nil, errors.New("output buffer limit")
	}
	return stdout.Bytes(), e
}
func Sandboxed(c Config) Execute {
	return func(ctx context.Context, dir, tool string, args []string) ([]byte, error) {
		pre := []string{"--job", dir, strconv.FormatInt(c.MaxOutput, 10), strconv.Itoa(int(c.ProcessTimeout.Seconds()) + 1), tool}
		return RunProcess(ctx, c.Sandbox, append(pre, args...), dir)
	}
}

// jpegQScale maps jpeg_quality 50–95 onto the MJPEG quantizer scale 31–2
// (lower is better quality and a larger file).
func jpegQScale(quality int) int {
	return 2 + int(math.Round(float64(95-quality)*29/45))
}

package service

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This direct executor exists only in tests. Production has no sandbox bypass.
func fixtureExecutor(ctx context.Context, dir, tool string, args []string) ([]byte, error) {
	return RunProcess(ctx, "/usr/bin/"+tool, args, dir)
}
func TestMediaOperations(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg not installed")
	}
	fixtures := t.TempDir()
	video := filepath.Join(fixtures, "video.mp4")
	still := filepath.Join(fixtures, "image.png")
	makeFixture := func(args ...string) {
		t.Helper()
		cmd := exec.Command("ffmpeg", append([]string{"-v", "error", "-y", "-threads", "1", "-filter_threads", "1"}, args...)...)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%v: %s", e, out)
		}
	}
	makeFixture("-f", "lavfi", "-i", "color=c=blue:s=64x64:r=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "1", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", "aac", video)
	makeFixture("-f", "lavfi", "-i", "color=c=red:s=64x64", "-frames:v", "1", "-threads", "1", still)
	cases := []struct{ op, format, params, source string }{
		{"metadata", "json", "{}", video}, {"transcode", "mp4", `{"width":32,"height":32}`, video},
		{"transcode", "webm", "{}", video}, {"resize", "png", `{"width":32,"height":32}`, still},
		{"resize", "jpg", `{"width":32,"height":32}`, still}, {"thumbnail", "jpg", `{"timestamp_seconds":0.1,"width":32,"height":32}`, video},
		{"extract_audio", "m4a", "{}", video}, {"extract_audio", "mp3", "{}", video}, {"extract_audio", "opus", "{}", video},
		{"trim", "mp4", `{"start_seconds":0.2,"duration_seconds":0.3}`, video}, {"remux", "mp4", "{}", video},
	}
	for _, tc := range cases {
		t.Run(tc.op+"_"+tc.format, func(t *testing.T) {
			dir := t.TempDir()
			data, _ := os.ReadFile(tc.source)
			os.WriteFile(filepath.Join(dir, "input"), data, 0600)
			r := request(data)
			r.Operation = tc.op
			r.Format = tc.format
			json.Unmarshal([]byte(tc.params), &r.Parameters)
			c := DefaultConfig()
			params, e := r.Validate(c)
			if e != nil {
				t.Fatal(e)
			}
			p := MediaProcessor{Config: c, Execute: fixtureExecutor}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			result, f := p.Process(ctx, dir, r, params)
			if f != nil {
				t.Fatalf("%+v", f)
			}
			if result.Size == 0 || len(result.SHA256) != 64 {
				t.Fatal(result)
			}
			if tc.op == "trim" {
				m, e := p.probe(ctx, dir, filepath.Join(dir, "output"))
				if e != nil || m.Duration > 0.6 {
					t.Fatalf("trim duration %v %v", m.Duration, e)
				}
			}
		})
	}
}
func TestMalformedMedia(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	data := []byte("#EXTM3U\nhttp://127.0.0.1/secret")
	os.WriteFile(filepath.Join(dir, "input"), data, 0600)
	c := DefaultConfig()
	p := MediaProcessor{Config: c, Execute: fixtureExecutor}
	r := request(data)
	_, f := p.Process(context.Background(), dir, r, Params{})
	if f == nil || strings.Contains(f.Message, "127.0.0.1") {
		t.Fatal(f)
	}
}
func TestProcessDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, e := RunProcess(ctx, "/bin/sleep", []string{"30"}, t.TempDir())
	if e == nil || time.Since(start) > 2*time.Second {
		t.Fatal("process did not terminate")
	}
}

func TestBufferIsBounded(t *testing.T) {
	b := &cappedBuffer{limit: 64}
	for i := 0; i < 100; i++ {
		n, e := b.Write([]byte(strings.Repeat("x", 32)))
		if e != nil || n != 32 {
			t.Fatal(n, e)
		}
	}
	if b.Len() != 64 {
		t.Fatal(b.Len())
	}
}

func TestDeclaredMIMERejected(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "input")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=s=64x64", "-frames:v", "1", "-threads", "1", "-f", "image2", "-update", "1", "-c:v", "png", path)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%s %v", out, e)
	}
	data, _ := os.ReadFile(path)
	r := request(data)
	r.declaredMIME = "image/jpeg"
	p := MediaProcessor{Config: DefaultConfig(), Execute: fixtureExecutor}
	_, f := p.Process(context.Background(), dir, r, Params{})
	if f == nil || f.Code != "unsupported_media" || f.Retryable {
		t.Fatal(f)
	}
}

func TestJPEGQualityScale(t *testing.T) {
	if jpegQScale(95) != 2 || jpegQScale(50) != 31 || jpegQScale(85) <= 2 {
		t.Fatal(jpegQScale(95), jpegQScale(85), jpegQScale(50))
	}
	args := buildArgs("in", "out", Request{Operation: "resize", Format: "jpg"}, Params{Width: 32, Height: 32, Fit: "contain", JPEGQuality: 50}, metadata{Width: 64, Height: 64}, DefaultConfig())
	if !strings.Contains(strings.Join(args, " "), "-q:v 31") {
		t.Fatal(args)
	}
}

func TestAsyncRangeAndRetryableCodes(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "input")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=s=64x64:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "mp4", path)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%s %v", out, e)
	}
	data, _ := os.ReadFile(path)
	r := request(data)
	r.Operation, r.Format = "thumbnail", "png"
	p := MediaProcessor{Config: DefaultConfig(), Execute: fixtureExecutor}
	_, f := p.Process(context.Background(), dir, r, Params{Width: 32, Height: 32, Fit: "contain", Timestamp: 5})
	if f == nil || f.Code != "range_out_of_bounds" || f.Field != "parameters.timestamp_seconds" {
		t.Fatal(f)
	}
	if !fault("processing_failed").Retryable || fault("invalid_media").Retryable {
		t.Fatal("retryable flags do not follow the contract")
	}
}

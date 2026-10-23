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

// runFixture validates and processes one request against a fixture file and
// returns the probed output.
func runFixture(t *testing.T, source, op, format, params string) (metadata, string) {
	t.Helper()
	dir := t.TempDir()
	data, _ := os.ReadFile(source)
	os.WriteFile(filepath.Join(dir, "input"), data, 0600)
	r := request(data)
	r.Operation = op
	r.Format = format
	json.Unmarshal([]byte(params), &r.Parameters)
	c := DefaultConfig()
	v, verr := r.Validate(c)
	if verr != nil {
		t.Fatal(verr)
	}
	p := MediaProcessor{Config: c, Execute: fixtureExecutor}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, f := p.Process(ctx, dir, r, v); f != nil {
		t.Fatalf("%s %s: %+v", op, params, f)
	}
	output := filepath.Join(dir, "output")
	var m metadata
	var e error
	if op == "metadata" {
		data, _ = os.ReadFile(output)
		e = json.Unmarshal(data, &m)
	} else {
		m, e = p.probe(ctx, dir, output)
	}
	if e != nil {
		t.Fatal(e)
	}
	return m, output
}

func TestVideoExtensions(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg not installed")
	}
	fixtures := t.TempDir()
	makeFixture := func(name string, args ...string) string {
		t.Helper()
		path := filepath.Join(fixtures, name)
		cmd := exec.Command("ffmpeg", append(append([]string{"-v", "error", "-y", "-threads", "1", "-filter_threads", "1"}, args...), path)...)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%v: %s", e, out)
		}
		return path
	}
	surround := makeFixture("surround.mp4", "-f", "lavfi", "-i", "testsrc=s=96x54:r=30:d=2", "-f", "lavfi", "-i", "sine=d=2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "6")
	slow := makeFixture("slow.mp4", "-f", "lavfi", "-i", "testsrc=s=64x64:r=10:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p")
	rotated := makeFixture("rotated.mp4", "-display_rotation", "90", "-i", surround, "-c", "copy")
	cover := makeFixture("cover.png", "-f", "lavfi", "-i", "color=c=red:s=32x32", "-frames:v", "1")
	withCover := makeFixture("cover.mp4", "-i", cover, "-i", surround, "-map", "0", "-map", "1", "-c", "copy", "-disposition:v:0", "attached_pic")
	tenBit := makeFixture("tenbit.mp4", "-f", "lavfi", "-i", "testsrc=s=64x64:r=10:d=1", "-c:v", "libx264", "-pix_fmt", "yuv420p10le")
	webm := makeFixture("clip.webm", "-f", "lavfi", "-i", "testsrc=s=64x64:r=10:d=1", "-f", "lavfi", "-i", "sine=d=1", "-c:v", "libvpx-vp9", "-c:a", "libopus")
	coverMP3 := makeFixture("cover.mp3", "-f", "lavfi", "-i", "sine=d=1", "-i", cover, "-map", "0", "-map", "1", "-c:a", "libmp3lame", "-c:v", "png", "-disposition:v:0", "attached_pic")

	t.Run("silent capped animation", func(t *testing.T) {
		m, _ := runFixture(t, surround, "transcode", "mp4", `{"audio_mode":"remove","max_dimension":48,"max_fps":15,"quality":28,"max_bitrate_kbps":200}`)
		if m.AudioCodec != "" || m.Width != 48 || m.Height != 26 || m.FrameRate > 15.01 {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("rotated source keeps orientation", func(t *testing.T) {
		if m, _ := runFixture(t, rotated, "transcode", "mp4", `{"max_dimension":48}`); m.Width != 26 || m.Height != 48 {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("never upscales or raises frame rate", func(t *testing.T) {
		m, _ := runFixture(t, slow, "transcode", "mp4", `{"max_dimension":640,"max_fps":15}`)
		if m.Width != 64 || m.Height != 64 || m.FrameRate < 9.9 || m.FrameRate > 10.1 {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("surround is downmixed", func(t *testing.T) {
		if m, _ := runFixture(t, surround, "transcode", "mp4", `{}`); m.Channels != 2 {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("10-bit source becomes High profile yuv420p", func(t *testing.T) {
		_, output := runFixture(t, tenBit, "transcode", "mp4", `{}`)
		out, e := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=profile,pix_fmt", "-of", "csv=p=0", output).Output()
		if e != nil || strings.TrimSpace(string(out)) != "High,yuv420p" {
			t.Fatalf("%q %v", out, e)
		}
	})
	t.Run("cover art is not the primary video", func(t *testing.T) {
		m, _ := runFixture(t, withCover, "metadata", "json", `{}`)
		if m.Width != 96 || m.Height != 54 {
			t.Fatalf("%+v", m)
		}
		if m, _ = runFixture(t, withCover, "transcode", "mp4", `{"audio_mode":"remove"}`); m.Width != 96 || m.Height != 54 || m.AudioCodec != "" {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("audio with cover art has no video", func(t *testing.T) {
		dir := t.TempDir()
		data, _ := os.ReadFile(coverMP3)
		os.WriteFile(filepath.Join(dir, "input"), data, 0600)
		r := request(data)
		r.Operation, r.Format = "transcode", "mp4"
		v, _ := r.Validate(DefaultConfig())
		p := MediaProcessor{Config: DefaultConfig(), Execute: fixtureExecutor}
		if _, f := p.Process(context.Background(), dir, r, v); f == nil || f.Code != "unsupported_media" {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("trim with geometry and no audio", func(t *testing.T) {
		m, _ := runFixture(t, surround, "trim", "mp4", `{"start_seconds":1,"duration_seconds":0.5,"audio_mode":"remove","max_dimension":32,"max_fps":12}`)
		if m.AudioCodec != "" || m.Width != 32 || m.Height != 18 || m.Duration > 0.8 || m.FrameRate > 12.01 {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("mute remux keeps the container", func(t *testing.T) {
		if m, _ := runFixture(t, surround, "remux", "mp4", `{"audio_mode":"remove"}`); m.AudioCodec != "" || m.VideoCodec != "h264" {
			t.Fatalf("%+v", m)
		}
		if m, _ := runFixture(t, webm, "remux", "webm", `{"audio_mode":"remove"}`); m.AudioCodec != "" || m.VideoCodec != "vp9" {
			t.Fatalf("%+v", m)
		}
	})
	t.Run("removed audio relaxes remux compatibility", func(t *testing.T) {
		vp9InMKV := makeFixture("vp9-aac.mkv", "-f", "lavfi", "-i", "testsrc=s=64x64:r=10:d=1", "-f", "lavfi", "-i", "sine=d=1", "-c:v", "libvpx-vp9", "-c:a", "aac")
		if m, _ := runFixture(t, vp9InMKV, "remux", "webm", `{"audio_mode":"remove"}`); m.AudioCodec != "" {
			t.Fatalf("%+v", m)
		}
	})
}

func TestExtensionArguments(t *testing.T) {
	c := DefaultConfig()
	m := metadata{Width: 1920, Height: 1080, FrameRate: 30, Channels: 6, VideoCodec: "h264"}
	trim := strings.Join(buildArgs("in", "out", Request{Operation: "trim", Format: "mp4"}, Params{Start: 5, Duration: 2, Quality: 28, Bitrate: 128, AudioMode: "remove", MaxDimension: 640, MaxFPS: 15, MaxBitrate: 800}, m, c), " ")
	for _, want := range []string{"-ss 5.000000 -v error", "-i in", "-t 2.000000", "-vf scale=640:360,fps=15,setsar=1", "-map 0:V:0 -an", "-preset medium", "-profile:v high", "-maxrate 800k -bufsize 1600k"} {
		if !strings.Contains(trim, want) {
			t.Fatalf("missing %q in %s", want, trim)
		}
	}
	if strings.Contains(trim, "0:a") || strings.Contains(trim, "-c:a") {
		t.Fatal(trim)
	}
	transcode := strings.Join(buildArgs("in", "out", Request{Operation: "transcode", Format: "mp4"}, Params{Quality: 23, Bitrate: 128, AudioMode: "preserve"}, m, c), " ")
	for _, want := range []string{"-map 0:V:0 -map 0:a:0?", "-preset veryfast", "-c:a aac -b:a 128k -ac 2"} {
		if !strings.Contains(transcode, want) {
			t.Fatalf("missing %q in %s", want, transcode)
		}
	}
	if strings.Contains(transcode, "-vf") || strings.Contains(transcode, "-maxrate") {
		t.Fatal(transcode)
	}
	for _, tc := range []struct{ w, h, limit, ew, eh int }{
		{1920, 1080, 640, 640, 360}, {1080, 1920, 640, 360, 640}, {96, 54, 48, 48, 26}, {64, 64, 640, 64, 64}, {65, 33, 640, 64, 32},
	} {
		if w, h := boundedSize(tc.w, tc.h, tc.limit); w != tc.ew || h != tc.eh {
			t.Fatalf("%dx%d/%d: got %dx%d", tc.w, tc.h, tc.limit, w, h)
		}
	}
	if frameRate("30000/1001") < 29.9 || frameRate("0/0") != 0 || frameRate("N/A") != 0 || frameRate("25") != 25 {
		t.Fatal("frame rate parsing")
	}
}

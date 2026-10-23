package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ffmpeg-service/internal/service"
)

func startup(c service.Config) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return errors.New("API socket unavailable")
	}
	err = syscall.Connect(fd, &syscall.SockaddrInet4{Port: 9, Addr: [4]byte{127, 0, 0, 1}})
	syscall.Close(fd)
	if !errors.Is(err, syscall.EPERM) {
		return errors.New("API egress guard missing; use the image entrypoint")
	}
	var stat syscall.Statfs_t
	if e := syscall.Statfs(c.WorkDir, &stat); e != nil {
		return e
	}
	if uint64(stat.Type) != 0x01021994 {
		return errors.New("WORK_DIR must be a dedicated tmpfs")
	}
	if uint64(stat.Bsize)*stat.Blocks < uint64(c.StorageBudget) {
		return errors.New("tmpfs smaller than storage budget")
	}
	entries, e := os.ReadDir(c.WorkDir)
	if e != nil {
		return e
	}
	for _, v := range entries {
		if !strings.HasPrefix(v.Name(), "job-") {
			return errors.New("unexpected content in dedicated WORK_DIR")
		}
		if e = os.RemoveAll(filepath.Join(c.WorkDir, v.Name())); e != nil {
			return e
		}
	}
	dir, e := os.MkdirTemp(c.WorkDir, "job-check-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	for _, f := range []string{"input", "output"} {
		if e = os.WriteFile(filepath.Join(dir, f), []byte("check"), 0600); e != nil {
			return e
		}
	}
	sentinel, e := os.CreateTemp(c.WorkDir, "job-protected-")
	if e != nil {
		return e
	}
	sentinel.Close()
	defer os.Remove(sentinel.Name())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, e = service.RunProcess(ctx, c.Sandbox, []string{"--selftest", dir, c.KeysFile, sentinel.Name()}, dir); e != nil {
		return errors.New("sandbox self-test failed; Landlock ABI >=3 and seccomp are mandatory")
	}
	run := service.Sandboxed(c)
	data, e := run(ctx, dir, "ffmpeg", []string{"-hide_banner", "-encoders"})
	if e != nil {
		return errors.New("sandboxed ffmpeg unavailable")
	}
	for _, codec := range []string{"libx264", "libvpx-vp9", "aac", "libopus", "libmp3lame", "png", "mjpeg"} {
		if !strings.Contains(string(data), codec) {
			return fmt.Errorf("missing encoder: %s", codec)
		}
	}
	_, e = run(ctx, dir, "ffprobe", []string{"-version"})
	if e != nil {
		return errors.New("sandboxed ffprobe unavailable")
	}
	return nil
}
func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		client := http.Client{Timeout: 2 * time.Second}
		r, e := client.Get("http://127.0.0.1:8080/health/ready")
		if e != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c, e := service.FromEnv()
	if e != nil {
		log.Error("configuration invalid", "reason", e.Error())
		os.Exit(1)
	}
	keys, e := service.LoadKeys(c.KeysFile)
	if e != nil {
		log.Error("API keys invalid")
		os.Exit(1)
	}
	if e = startup(c); e != nil {
		log.Error("startup refused", "reason", e.Error())
		os.Exit(1)
	}
	s, e := service.New(c, keys, &service.MediaProcessor{Config: c, Execute: service.Sandboxed(c)})
	if e != nil {
		log.Error("service initialization failed")
		os.Exit(1)
	}
	server := &http.Server{Addr: c.Addr, Handler: s, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = server.Close()
		s.Close()
		close(done)
	}()
	log.Info("listening", "address", c.Addr)
	if e = server.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
		log.Error("HTTP server failed")
		s.Close()
		os.Exit(1)
	}
	<-done
}

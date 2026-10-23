package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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
		os.Exit(healthcheck())
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
	listeners, e := listen(c, log)
	if e != nil {
		log.Error("listen failed", "reason", e.Error())
		s.Close()
		os.Exit(1)
	}
	failed := make(chan error, len(listeners))
	for _, l := range listeners {
		log.Info("listening", "address", l.Addr().String())
		go func(l net.Listener) {
			if e := server.Serve(l); e != nil && !errors.Is(e, http.ErrServerClosed) {
				failed <- e
			}
		}(l)
	}
	select {
	case <-failed:
		log.Error("HTTP server failed")
		s.Close()
		os.Exit(1)
	case <-done:
	}
}

// listen opens the TCP listener (restricted to loopback, attached Docker
// networks and WHITELIST_IPS) and the optional Unix socket.
func listen(c service.Config, log *slog.Logger) ([]net.Listener, error) {
	var out []net.Listener
	if c.Addr != "off" {
		l, e := net.Listen("tcp", c.Addr)
		if e != nil {
			return nil, e
		}
		policy := service.NewAccessPolicy(c.Whitelist)
		out = append(out, &service.FilteredListener{Listener: l, Allow: policy.Allowed, Log: log})
	}
	if c.UnixSocket != "" {
		if info, e := os.Lstat(c.UnixSocket); e == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return nil, errors.New("UNIX_SOCKET path exists and is not a socket")
			}
			_ = os.Remove(c.UnixSocket) // stale socket from a previous run
		}
		l, e := net.Listen("unix", c.UnixSocket)
		if e != nil {
			return nil, e
		}
		// Owner and group only: share the socket with the client container
		// through a volume and a common group.
		if e = os.Chmod(c.UnixSocket, 0660); e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, nil
}

// healthcheck queries readiness over whichever listener is configured.
func healthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	socket := os.Getenv("UNIX_SOCKET")
	transport := &http.Transport{}
	url := ""
	switch {
	case addr != "off":
		_, port, e := net.SplitHostPort(addr)
		if addr == "" || e != nil || port == "" {
			port = "8080"
		}
		url = "http://127.0.0.1:" + port + "/health/ready"
	case socket != "":
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}
		url = "http://unix/health/ready"
	default:
		return 1
	}
	client := http.Client{Timeout: 2 * time.Second, Transport: transport}
	r, e := client.Get(url)
	if e != nil {
		return 1
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		return 1
	}
	return 0
}

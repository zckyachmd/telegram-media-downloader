package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/botnick/telegram-media-downloader/core-service/internal/app"
	"github.com/botnick/telegram-media-downloader/core-service/internal/backup"
	"github.com/botnick/telegram-media-downloader/core-service/internal/config"
	"github.com/botnick/telegram-media-downloader/core-service/internal/version"
	"github.com/botnick/telegram-media-downloader/core-service/internal/webassets"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "setup":
			return setupDashboard(args[1:], os.Stdin, stdout, stderr)
		case "backup-restore":
			return backupRestore(args[1:], stdout, stderr)
		case "backup-decrypt":
			return backupDecrypt(args[1:], stdout, stderr)
		case "backup-oauth":
			return backupOAuth(args[1:], os.Stdin, stdout, stderr)
		case "version", "--version", "-v":
			_, _ = fmt.Fprintf(stdout, "tgdl-server %s %s/%s %s\n", version.AppVersion, runtime.GOOS, runtime.GOARCH, runtime.Version())
			return 0
		case "help", "--help", "-h":
			_, _ = io.WriteString(stdout, "usage: tgdl-server [version | setup --help | backup-decrypt --help | backup-restore --help | backup-oauth gdrive|dropbox]\nTGDL_DATA_DIR is required for the server.\n")
			return 0
		}
	}
	cfg, err := config.FromFullEnv(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	downloadsDir := cfg.DownloadsDir
	if downloadsDir == "" {
		downloadsDir = filepath.Join(cfg.DataDir, "downloads")
	}
	if _, err = backup.ActivatePendingFullRestore(cfg.DataDir, downloadsDir); err != nil {
		_, _ = fmt.Fprintln(stderr, "recover pending full restore:", err)
		return 1
	}
	devMode := os.Getenv("TGDL_DEV_MODE") == "1"
	var static fs.FS
	if devMode {
		static = os.DirFS("internal/webassets/public")
	} else {
		static, err = fs.Sub(webassets.FS, "public")
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpOptions, err := app.HTTPOptionsFromEnv(os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	mcpDisabled := os.Getenv("TGDL_MCP_DISABLED") == "1"
	mcpPort := 3001
	if !mcpDisabled {
		if raw := strings.TrimSpace(os.Getenv("TGDL_MCP_PORT")); raw != "" {
			mcpPort, err = strconv.Atoi(raw)
			if err != nil || mcpPort < 1 || mcpPort > 65535 {
				_, _ = fmt.Fprintln(stderr, "TGDL_MCP_PORT must be between 1 and 65535")
				return 2
			}
		}
	}
	mcpIssuer := strings.TrimRight(strings.TrimSpace(os.Getenv("TGDL_MCP_ISSUER")), "/")
	if mcpIssuer == "" {
		mcpIssuer = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(mcpPort))
	}
	a, err := app.New(ctx, app.Config{HTTP: httpOptions, DataDir: cfg.DataDir, DownloadsDir: cfg.DownloadsDir, Port: cfg.Port, Static: static, CookieName: cfg.CookieName, SessionTTL: cfg.SessionTTL, Output: stdout, Restart: stop, SecureCookies: os.Getenv("TGDL_SECURE_COOKIES") == "1", DevMode: devMode, MCPIssuer: mcpIssuer})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	address := net.JoinHostPort(cfg.BindHost, strconv.Itoa(cfg.Port))
	log.Printf("tgdl-server listening on %s", address)
	server := &http.Server{Addr: address, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	var serveErr error
	if mcpDisabled {
		log.Printf("MCP listener disabled")
		serveErr = serve(ctx, server)
	} else {
		mcpAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(mcpPort))
		if bindHost := strings.TrimSpace(os.Getenv("TGDL_MCP_BIND_HOST")); bindHost != "" {
			mcpAddress = net.JoinHostPort(bindHost, strconv.Itoa(mcpPort))
		}
		log.Printf("tgdl MCP listening on %s", mcpAddress)
		mcpServer := &http.Server{Addr: mcpAddress, Handler: a.MCPHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
		serveErr = serveTogether(ctx, server, mcpServer)
	}
	closeErr := a.Close()
	if err = errors.Join(serveErr, closeErr); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	activated, err := backup.ActivatePendingFullRestore(cfg.DataDir, downloadsDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "activate full restore:", err)
		return 1
	}
	if activated {
		executable, err := os.Executable()
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "restart after full restore:", err)
			return 1
		}
		child := exec.Command(executable, args...)
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, stdout, stderr
		if err = child.Start(); err != nil {
			_, _ = fmt.Fprintln(stderr, "restart after full restore:", err)
			return 1
		}
		_ = child.Process.Release()
	}
	return 0
}

func serveTogether(ctx context.Context, servers ...*http.Server) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(servers))
	for _, server := range servers {
		go func(server *http.Server) { done <- serve(runCtx, server) }(server)
	}
	var errs []error
	for range servers {
		err := <-done
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
			cancel()
		}
	}
	return errors.Join(errs...)
}

func serve(ctx context.Context, server *http.Server) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(sanitizeListener{Listener: listener}) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := server.Shutdown(shutdown)
		if err != nil {
			_ = server.Close()
		}
		listenErr := <-done
		if errors.Is(listenErr, http.ErrServerClosed) {
			listenErr = nil
		}
		return errors.Join(err, listenErr)
	}
}

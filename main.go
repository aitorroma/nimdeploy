// nimdeploy receives GitHub push webhooks and runs a deploy command per
// repository, keeping one log file per run plus a queryable status.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const usage = `Usage:
  nimdeploy install                     install for the current user, no root needed
                                        (~/.local/bin, ~/.config/nimdeploy, systemd user service)
  nimdeploy uninstall [--purge]         remove a user install
  nimdeploy [flags] serve               run the webhook server (what the service does;
                                        also the default without a command and terminal)
  nimdeploy [flags] run [-commit SHA] [-f] <deploy>
                                        start a deploy through the running server
  nimdeploy [flags] status [-json] [deploy]
                                        show deploy status
  nimdeploy [flags] history [-n 20] [-json] [deploy]
                                        list past deploys (kept logs), newest first
  nimdeploy [flags] nginx [-api]        print nginx location blocks for the hooks

Flags:
`

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	configPath := flag.String("config", defaultConfigPath(), "path to the TOML config ($NIMDEPLOY_CONFIG)")
	envFile := flag.String("env-file", "", "secrets file the CLI reads the API token from (default: secrets.env next to the config)")
	check := flag.Bool("check", false, "validate the config and secrets, then exit")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *envFile == "" {
		*envFile = filepath.Join(filepath.Dir(*configPath), "secrets.env")
	}
	if *showVersion {
		fmt.Println("nimdeploy", version)
		return
	}
	// These work before any config exists.
	if args := flag.Args(); len(args) > 0 {
		switch args[0] {
		case "install":
			os.Exit(cliInstall(args[1:]))
		case "uninstall":
			os.Exit(cliUninstall(args[1:]))
		}
	}

	cfg, err := LoadConfig(*configPath)
	if errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("config %s not found: install with ./install.sh (as root, or as a normal user for ~/.config/nimdeploy), or pass -config / $NIMDEPLOY_CONFIG", *configPath)
	}
	if err != nil {
		log.Fatalf("config %s: %v", *configPath, err)
	}

	args := flag.Args()
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "serve":
		// Typed with no command in a terminal, it's almost never meant to
		// start a second server next to the service: show what to do.
		// systemd starts it without a terminal; "serve" forces it.
		if cmd == "" && !*check && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
			fmt.Printf("nimdeploy %s. The server runs as a service; from a terminal you probably want:\n\n"+
				"  nimdeploy status            last deploy of each app\n"+
				"  nimdeploy history           past deploys\n"+
				"  nimdeploy run -f <deploy>   deploy now and follow the log\n"+
				"  systemctl status nimdeploy  the service (user installs: systemctl --user status nimdeploy)\n\n"+
				"Run the server in the foreground with: nimdeploy serve   (all options: nimdeploy -h)\n", version)
			return
		}
		if err := cfg.ResolveSecrets(); err != nil {
			log.Fatalf("config %s: %v", *configPath, err)
		}
		if *check {
			fmt.Printf("config OK: %d deploys\n", len(cfg.Deploy))
			return
		}
		serve(*configPath, cfg)
	case "run":
		os.Exit(cliRun(cfg, *envFile, args))
	case "status":
		os.Exit(cliStatus(cfg, *envFile, args))
	case "history":
		os.Exit(cliHistory(cfg, *envFile, args))
	case "nginx":
		os.Exit(cliNginx(cfg, args))
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// handlerSwap lets SIGHUP replace the routes without restarting the listener.
type handlerSwap struct{ h atomic.Value }

func (s *handlerSwap) Store(h http.Handler) { s.h.Store(&h) }

func (s *handlerSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load().(*http.Handler)).ServeHTTP(w, r)
}

func serve(configPath string, cfg *Config) {
	runner, err := NewRunner(cfg, NewNotifier(cfg))
	if err != nil {
		log.Fatalf("runner: %v", err)
	}
	handler := &handlerSwap{}
	handler.Store(NewServer(cfg, runner).Routes())

	ln, err := listen(cfg.Server)
	if err != nil {
		log.Fatalf("listen %s: %v", cfg.Server.Listen, err)
	}
	if cfg.Server.apiToken == "" && !isLocal(cfg.Server) {
		log.Printf("warning: listening on %s without server.api_token_env: /status is readable by anyone who can reach it", cfg.Server.Listen)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logDeploys(cfg)
		log.Printf("nimdeploy %s listening on %s, logs in %s", version, cfg.Server.Listen, cfg.Logging.Directory)
		errCh <- srv.Serve(ln)
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
loop:
	for {
		select {
		case sig := <-sigs:
			if sig != syscall.SIGHUP {
				log.Printf("shutting down")
				break loop
			}
			if next, err := reloadConfig(configPath, cfg); err != nil {
				log.Printf("reload failed, keeping current config: %v", err)
			} else {
				cfg = next
				runner.SetConfig(cfg, NewNotifier(cfg))
				handler.Store(NewServer(cfg, runner).Routes())
				logDeploys(cfg)
				log.Printf("config reloaded: %d deploys", len(cfg.Deploy))
			}
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("server: %v", err)
			}
			break loop
		}
	}
	signal.Stop(sigs)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	runner.Shutdown(cfg.Server.ShutdownTimeout.Duration)
}

func reloadConfig(path string, current *Config) (*Config, error) {
	next, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if err := next.ResolveSecrets(); err != nil {
		return nil, fmt.Errorf("%w (new secrets need a restart: systemd only reads secrets.env on start)", err)
	}
	if next.Server.Listen != current.Server.Listen {
		return nil, fmt.Errorf("server.listen changed: restart needed")
	}
	if next.Logging.Directory != current.Logging.Directory {
		return nil, fmt.Errorf("logging.directory changed: restart needed")
	}
	return next, nil
}

func logDeploys(cfg *Config) {
	for _, name := range cfg.DeployNames() {
		d := cfg.Deploy[name]
		log.Printf("deploy=%s path=%s repository=%s branch=%s", name, d.Path, d.Repository, d.Branch)
	}
}

// listen opens the TCP address or unix socket from server.listen.
func listen(s ServerConfig) (net.Listener, error) {
	if s.socketPath == "" {
		return net.Listen("tcp", s.Listen)
	}
	// Remove a socket left behind by a crash, but never a regular file.
	if info, err := os.Lstat(s.socketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(s.socketPath)
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.socketPath, s.socketMode); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func isLocal(s ServerConfig) bool {
	if s.socketPath != "" {
		return true
	}
	host, _, _ := net.SplitHostPort(s.Listen)
	addr, err := netip.ParseAddr(host)
	return host == "localhost" || (err == nil && addr.IsLoopback())
}

// defaultConfigPath is $NIMDEPLOY_CONFIG, else the per-user config of a
// user-mode install (~/.config/nimdeploy/config.toml) when not root and it
// exists, else /etc/nimdeploy/config.toml.
func defaultConfigPath() string {
	if p := os.Getenv("NIMDEPLOY_CONFIG"); p != "" {
		return p
	}
	if os.Geteuid() != 0 {
		if dir, err := os.UserConfigDir(); err == nil {
			p := filepath.Join(dir, "nimdeploy", "config.toml")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return "/etc/nimdeploy/config.toml"
}

// isTerminal reports whether f is a terminal (TCGETS succeeds), unlike
// /dev/null, which systemd gives services as stdin.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

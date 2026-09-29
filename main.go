// nimdeploy receives GitHub push webhooks and runs a deploy command per
// repository, keeping one log file per run plus a queryable status.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const usage = `Usage:
  nimdeploy [flags]                     run the webhook server
  nimdeploy [flags] run [-commit SHA] [-f] <deploy>
                                        start a deploy through the running server
  nimdeploy [flags] status [-json] [deploy]
                                        show deploy status

Flags:
`

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	configPath := flag.String("config", "/etc/nimdeploy/config.toml", "path to the TOML config")
	envFile := flag.String("env-file", "/etc/nimdeploy/secrets.env", "secrets file the CLI reads the API token from")
	check := flag.Bool("check", false, "validate the config and secrets, then exit")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("nimdeploy", version)
		return
	}

	cfg, err := LoadConfig(*configPath)
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

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
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
		errCh <- srv.ListenAndServe()
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

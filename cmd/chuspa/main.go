// chuspa es el binario del motor OpenChuspa.
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
	"syscall"
	"time"

	"github.com/OpenChuspa/OpenChuspa-core/internal/auth"
	"github.com/OpenChuspa/OpenChuspa-core/internal/config"
	"github.com/OpenChuspa/OpenChuspa-core/internal/httpx"
	"github.com/OpenChuspa/OpenChuspa-core/internal/s3api"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage/fsbolt"
)

// version se fija al compilar con -ldflags "-X main.version=...".
var version = "dev"

const usage = `chuspa: motor de almacenamiento compatible con S3

Uso:
  chuspa serve         arranca el servidor
  chuspa healthcheck   consulta la salud del servidor local (para contenedores)
  chuspa version       muestra la versión

La configuración se lee de variables de entorno CHUSPA_*; ver README.md.
`

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "version", "--version", "-v":
		fmt.Println(version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "chuspa:", err)
		os.Exit(1)
	}
}

func serve() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, nil)
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, nil)
	}
	log := slog.New(handler)

	backend, orphans, err := fsbolt.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer backend.Close()
	if orphans > 0 {
		log.Info("se limpiaron restos de una ejecución anterior", "archivos", orphans)
	}

	api := &s3api.Server{Backend: backend, Region: cfg.Region, Log: log}
	if cfg.RootUser != "" {
		api.Verifier = &auth.Verifier{AccessKey: cfg.RootUser, SecretKey: cfg.RootPassword, Region: cfg.Region}
	}
	if cfg.AllowAnonymous {
		api.Verifier = nil
		log.Warn("MODO ANÓNIMO: el servidor acepta peticiones sin autenticación; solo para pruebas locales")
	}
	if !cfg.Loopback() {
		log.Warn("el servidor escucha fuera de loopback sin TLS propio: póngale delante un proxy con TLS", "addr", cfg.Addr)
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	srv := httpx.NewServer(httpx.Wrap(api, log, func(w http.ResponseWriter, r *http.Request) {
		s3api.WriteError(w, r, s3api.ErrInternal)
	}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(httpx.LimitListener(listener, httpx.MaxConnections)) }()
	log.Info("OpenChuspa escuchando", "addr", listener.Addr().String(), "region", cfg.Region,
		"data_dir", cfg.DataDir, "version", version, "anonymous", api.Verifier == nil)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	// Apagado ordenado: se dejan terminar las peticiones en curso.
	log.Info("apagando: se esperan las peticiones en curso")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck existe porque la imagen final no trae shell ni curl.
func healthcheck() error {
	addr := os.Getenv("CHUSPA_ADDR")
	if addr == "" {
		addr = config.DefaultAddr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + s3api.HealthPath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("el servidor respondió %d", resp.StatusCode)
	}
	return nil
}

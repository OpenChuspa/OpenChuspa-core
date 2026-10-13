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
  chuspa serve                            arranca el servidor
  chuspa healthcheck                      consulta la salud del servidor local (para contenedores)
  chuspa inspect [chuspa [prefijo]]       lista chuspas u objetos leyendo el índice
  chuspa export <chuspa> <key> <archivo>  copia el contenido de un objeto a un archivo
  chuspa version                          muestra la versión

inspect y export leen CHUSPA_DATA_DIR directamente y necesitan el servidor detenido.

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
	case "inspect":
		err = inspect(os.Args[2:])
	case "export":
		err = export(os.Args[2:])
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

	go janitor(ctx, backend, cfg.MultipartExpiry, log)

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

// Márgenes del mantenimiento periódico. Un blob entra al índice instantes
// después de publicarse, y una subida viva sigue escribiendo en su archivo de
// staging: pasados estos tiempos, lo que no tenga referencia es basura.
const (
	sweepInterval  = time.Hour
	backupInterval = 24 * time.Hour
	blobGrace      = time.Hour
	stagingGrace   = 24 * time.Hour
)

// janitor borra huérfanos, aborta subidas multipart abandonadas y deja una
// copia del índice, mientras el servidor atiende peticiones.
func janitor(ctx context.Context, backend *fsbolt.Backend, uploadTTL time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	var lastBackup time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			res, err := backend.Sweep(now, blobGrace, stagingGrace, uploadTTL)
			switch {
			case err != nil:
				log.Error("falló el mantenimiento", "error", err.Error())
			case res.Orphans > 0 || res.ExpiredUploads > 0:
				log.Info("mantenimiento", "huerfanos", res.Orphans, "subidas_expiradas", res.ExpiredUploads)
			}
			if now.Sub(lastBackup) >= backupInterval {
				if err := backend.Backup(); err != nil {
					log.Error("falló la copia del índice", "error", err.Error())
				} else {
					lastBackup = now
				}
			}
		}
	}
}

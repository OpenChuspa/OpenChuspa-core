// chuspa es el binario del motor OpenChuspa.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/OpenChuspa/OpenChuspa-core/internal/auth"
	"github.com/OpenChuspa/OpenChuspa-core/internal/config"
	"github.com/OpenChuspa/OpenChuspa-core/internal/httpx"
	"github.com/OpenChuspa/OpenChuspa-core/internal/metrics"
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
  chuspa verify                           relee todo el contenido y lo compara con su hash
  chuspa rebuild-index                    reconstruye el índice desde los blobs y sus metadatos
  chuspa version                          muestra la versión

inspect, export, verify y rebuild-index leen CHUSPA_DATA_DIR directamente y
necesitan el servidor detenido. rebuild-index no borra nada: renombra el índice
anterior y aparta en recuperados/ lo que no pueda asignar a un objeto.

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
	case "verify":
		err = verifyCmd(os.Args[2:])
	case "rebuild-index":
		err = rebuildIndex(os.Args[2:])
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
	backend.MinFreeBytes = cfg.MinFreeBytes
	if orphans > 0 {
		log.Info("se limpiaron restos de una ejecución anterior", "archivos", orphans)
	}

	reg := metrics.New(version)
	st := newOpsState(reg, cfg.DataDir)
	api := &s3api.Server{Backend: backend, Region: cfg.Region, Log: log, OnAuthBlock: func() { st.authBlocks.Add(1) }}
	if cfg.RootUser != "" {
		api.Verifier = &auth.Verifier{AccessKey: cfg.RootUser, SecretKey: cfg.RootPassword,
			PreviousSecretKey: cfg.PreviousRootPassword, Region: cfg.Region}
		if cfg.AuthMaxFailures > 0 {
			api.Limiter = s3api.NewAuthLimiter()
			api.Limiter.MaxFailures = cfg.AuthMaxFailures
		}
		if cfg.PreviousRootPassword != "" {
			log.Warn("rotación en curso: se acepta también el secreto anterior; quite CHUSPA_ROOT_PASSWORD_PREVIOUS cuando todos los clientes usen el nuevo")
		}
	}
	if cfg.AllowAnonymous {
		api.Verifier = nil
		log.Warn("MODO ANÓNIMO: el servidor acepta peticiones sin autenticación; solo para pruebas locales")
	}
	if !cfg.Loopback() && cfg.TLSCertFile == "" {
		log.Warn("el servidor escucha fuera de loopback sin TLS: configure CHUSPA_TLS_CERT_FILE y CHUSPA_TLS_KEY_FILE o póngale delante un proxy con TLS", "addr", cfg.Addr)
	}

	tcp, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	listener := httpx.LimitListener(tcp, httpx.MaxConnections)
	if cfg.TLSCertFile != "" {
		certs, err := httpx.NewCertReloader(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return err
		}
		listener = tls.NewListener(listener, certs.TLSConfig())
	}
	srv := httpx.NewServer(httpx.Wrap(api, log, func(w http.ResponseWriter, r *http.Request) {
		s3api.WriteError(w, r, s3api.ErrInternal)
	}, reg.ObserveRequest))

	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		ml, err := net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			return fmt.Errorf("métricas: %w", err)
		}
		metricsSrv = &http.Server{Handler: reg, ReadHeaderTimeout: httpx.ReadHeaderTimeout}
		go metricsSrv.Serve(ml)
		log.Info("métricas de Prometheus en /metrics", "addr", ml.Addr().String())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(listener) }()
	log.Info("OpenChuspa escuchando", "addr", listener.Addr().String(), "region", cfg.Region,
		"data_dir", cfg.DataDir, "version", version, "anonymous", api.Verifier == nil, "tls", cfg.TLSCertFile != "")

	go janitor(ctx, backend, cfg.MultipartExpiry, log, st)
	if cfg.ScrubInterval > 0 {
		go scrubber(ctx, backend, cfg.ScrubInterval, log, st)
	}

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}
	// Apagado ordenado: se dejan terminar las peticiones en curso.
	log.Info("apagando: se esperan las peticiones en curso")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if metricsSrv != nil {
		metricsSrv.Close()
	}
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
	scheme, client := "http", &http.Client{Timeout: 3 * time.Second}
	if os.Getenv("CHUSPA_TLS_CERT_FILE") != "" {
		// Solo comprueba que el proceso local responde: la identidad del
		// certificado no importa aquí y puede no cubrir 127.0.0.1.
		scheme = "https"
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // conexión local de salud
	}
	resp, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + s3api.HealthPath)
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
func janitor(ctx context.Context, backend *fsbolt.Backend, uploadTTL time.Duration, log *slog.Logger, st *opsState) {
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
				st.maintenanceErrors.Add(1)
				log.Error("falló el mantenimiento", "error", err.Error())
			case res.Orphans > 0 || res.ExpiredUploads > 0:
				log.Info("mantenimiento", "huerfanos", res.Orphans, "subidas_expiradas", res.ExpiredUploads)
			}
			if now.Sub(lastBackup) >= backupInterval {
				if err := backend.Backup(); err != nil {
					st.maintenanceErrors.Add(1)
					log.Error("falló la copia del índice", "error", err.Error())
				} else {
					lastBackup = now
					st.lastBackup.Store(now.Unix())
				}
			}
		}
	}
}

// scrubber relee todo el contenido cada interval y registra como error cada
// objeto que no coincide con su hash: es la forma de enterarse de un disco que
// se está dañando antes de que alguien lea el objeto.
func scrubber(ctx context.Context, backend *fsbolt.Backend, interval time.Duration, log *slog.Logger, st *opsState) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			start := time.Now()
			problems := 0
			res, err := backend.Verify(ctx, func(p fsbolt.Problem) {
				problems++
				log.Error("objeto dañado", "chuspa", p.Bucket, "key", p.Key, "blob", p.Blob, "motivo", p.Reason)
			})
			if err != nil && ctx.Err() == nil {
				st.maintenanceErrors.Add(1)
				log.Error("falló la verificación del contenido", "error", err.Error())
				continue
			}
			st.scrubDamaged.Store(int64(problems))
			st.lastScrub.Store(time.Now().Unix())
			log.Info("verificación del contenido", "objetos", res.Objects, "bytes", res.Bytes,
				"dañados", problems, "duracion", time.Since(start).Round(time.Second).String())
		}
	}
}

// opsState es lo que el mantenimiento reporta a las métricas.
type opsState struct {
	maintenanceErrors, authBlocks *metrics.Counter
	lastBackup, lastScrub         atomic.Int64 // unix; 0 si todavía no ocurrió
	scrubDamaged                  atomic.Int64
}

func newOpsState(reg *metrics.Metrics, dataDir string) *opsState {
	st := &opsState{
		maintenanceErrors: reg.Counter("chuspa_maintenance_errors_total", "Fallos del mantenimiento, de la copia del índice o de la verificación."),
		authBlocks:        reg.Counter("chuspa_auth_blocks_total", "IPs bloqueadas por acumular autenticaciones fallidas."),
	}
	reg.GaugeFunc("chuspa_disk_free_bytes", "Espacio libre en el disco de los datos.", func() float64 {
		free, _ := fsbolt.FreeBytes(dataDir)
		return float64(free)
	})
	reg.GaugeFunc("chuspa_last_backup_timestamp_seconds", "Última copia del índice en meta.db.bak (0: ninguna desde el arranque).",
		func() float64 { return float64(st.lastBackup.Load()) })
	reg.GaugeFunc("chuspa_last_scrub_timestamp_seconds", "Última verificación completa del contenido (0: ninguna desde el arranque).",
		func() float64 { return float64(st.lastScrub.Load()) })
	reg.GaugeFunc("chuspa_scrub_damaged_objects", "Objetos dañados en la última verificación.",
		func() float64 { return float64(st.scrubDamaged.Load()) })
	return st
}

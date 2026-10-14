// Package httpx reúne el endurecimiento HTTP del servidor: identificador de
// petición, registro, recuperación de pánicos y límite de conexiones.
package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"
)

// Límites por defecto del servidor HTTP.
const (
	ReadHeaderTimeout = 10 * time.Second
	IdleTimeout       = 120 * time.Second
	MaxHeaderBytes    = 64 << 10
	MaxConnections    = 1000
)

const requestIDHeader = "x-amz-request-id"

// recorder guarda el estado y los bytes de la respuesta para el log.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Observer recibe cada petición atendida, para las métricas.
type Observer func(method string, status int, d time.Duration, in, out int64)

// Wrap envuelve next con el identificador de petición, el log de acceso y la
// recuperación de pánicos. onPanic escribe la respuesta de error si todavía
// no se ha enviado nada; observe, si no es nil, recibe cada petición.
//
// El log registra método, ruta, estado, bytes y duración. Nunca registra
// headers ni la query: ahí viajan Authorization y las firmas.
func Wrap(next http.Handler, log *slog.Logger, onPanic func(http.ResponseWriter, *http.Request), observe Observer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := newRequestID()
		w.Header().Set(requestIDHeader, id)
		w.Header().Set("Server", "OpenChuspa")
		rec := &recorder{ResponseWriter: w}

		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				log.Error("pánico en el handler", "request_id", id, "panic", p, "stack", string(debug.Stack()))
				if rec.status == 0 {
					onPanic(rec, r)
				}
			}
			elapsed := time.Since(start)
			log.Info("petición", "request_id", id, "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "bytes", rec.bytes, "duration_ms", elapsed.Milliseconds(),
				"remote", r.RemoteAddr)
			if observe != nil {
				observe(r.Method, rec.status, elapsed, r.ContentLength, rec.bytes)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

func newRequestID() string {
	var raw [8]byte
	rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// NewServer crea un http.Server con los tiempos y tamaños máximos del motor.
// No fija tiempos de lectura ni de escritura del cuerpo porque las subidas y
// descargas grandes son legítimamente largas.
func NewServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: ReadHeaderTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}

// LimitListener acepta como máximo n conexiones simultáneas; las demás
// esperan en la cola del sistema operativo.
func LimitListener(l net.Listener, n int) net.Listener {
	return &limitListener{Listener: l, slots: make(chan struct{}, n)}
}

type limitListener struct {
	net.Listener
	slots chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

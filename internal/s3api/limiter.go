package s3api

import (
	"net"
	"sync"
	"time"
)

// AuthLimiter frena la fuerza bruta contra las credenciales: una IP que
// acumula MaxFailures autenticaciones fallidas dentro de Window queda
// bloqueada durante Block y recibe 503 SlowDown sin que se verifique nada.
//
// Detrás de un proxy todas las peticiones llegan con la IP del proxy: un
// ataque bloquea a todos. Es preferible a dejar probar llaves sin límite.
type AuthLimiter struct {
	MaxFailures int
	Window      time.Duration
	Block       time.Duration
	Now         func() time.Time // reloj inyectable para pruebas

	mu      sync.Mutex
	clients map[string]*authFailures
}

type authFailures struct {
	count        int
	since        time.Time
	blockedUntil time.Time
}

// maxTrackedClients acota la memoria: por encima, se olvidan los vencidos.
const maxTrackedClients = 10000

// NewAuthLimiter crea el limitador con los valores por defecto: 20 fallos en
// un minuto bloquean durante 5 minutos.
func NewAuthLimiter() *AuthLimiter {
	return &AuthLimiter{MaxFailures: 20, Window: time.Minute, Block: 5 * time.Minute}
}

func (l *AuthLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Blocked indica si la dirección remota está bloqueada.
func (l *AuthLimiter) Blocked(remoteAddr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.clients[clientIP(remoteAddr)]
	return f != nil && l.now().Before(f.blockedUntil)
}

// Fail registra un fallo y devuelve true si con él la dirección queda
// bloqueada.
func (l *AuthLimiter) Fail(remoteAddr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.clients == nil {
		l.clients = map[string]*authFailures{}
	}
	ip := clientIP(remoteAddr)
	f := l.clients[ip]
	if f == nil || now.Sub(f.since) > l.Window {
		if len(l.clients) >= maxTrackedClients {
			l.forgetExpired(now)
		}
		f = &authFailures{since: now}
		l.clients[ip] = f
	}
	f.count++
	if f.count == l.MaxFailures {
		f.blockedUntil = now.Add(l.Block)
		return true
	}
	return false
}

func (l *AuthLimiter) forgetExpired(now time.Time) {
	for ip, f := range l.clients {
		if now.Sub(f.since) > l.Window && !now.Before(f.blockedUntil) {
			delete(l.clients, ip)
		}
	}
}

func clientIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

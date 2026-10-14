package httpx

import (
	"crypto/tls"
	"os"
	"sync"
	"time"
)

// certCheckInterval es cada cuánto se revisa si el certificado cambió en disco.
var certCheckInterval = 30 * time.Second

// CertReloader sirve el certificado de un par de archivos y lo vuelve a leer
// cuando cambian, para renovarlo (Let's Encrypt, cert-manager) sin reiniciar.
// Si la versión nueva no carga, se sigue sirviendo la anterior.
type CertReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

// NewCertReloader carga el certificado por primera vez.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *CertReloader) load() error {
	mod := r.lastModified()
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	r.cert, r.modTime = &cert, mod
	return nil
}

func (r *CertReloader) lastModified() time.Time {
	var latest time.Time
	for _, f := range []string{r.certFile, r.keyFile} {
		if info, err := os.Stat(f); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

// GetCertificate es para tls.Config.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := time.Now(); now.Sub(r.checked) >= certCheckInterval {
		r.checked = now
		if !r.lastModified().Equal(r.modTime) {
			_ = r.load()
		}
	}
	return r.cert, nil
}

// TLSConfig arma la configuración del servidor: TLS 1.2 como mínimo.
func (r *CertReloader) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate}
}

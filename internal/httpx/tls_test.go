package httpx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCert genera un certificado autofirmado para 127.0.0.1.
func writeCert(t *testing.T, dir, cn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, typ string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("cert.pem", "CERTIFICATE", der)
	write("key.pem", "EC PRIVATE KEY", keyDER)
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func TestCertReloader(t *testing.T) {
	old := certCheckInterval
	certCheckInterval = 0
	t.Cleanup(func() { certCheckInterval = old })

	dir := t.TempDir()
	first := writeCert(t, dir, "primero")
	r, err := NewCertReloader(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	go srv.Serve(tls.NewListener(ln, r.TLSConfig()))
	t.Cleanup(func() { srv.Close() })

	served := func(trust *x509.Certificate) string {
		t.Helper()
		pool := x509.NewCertPool()
		pool.AddCert(trust)
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
		resp, err := c.Get("https://" + ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.TLS.PeerCertificates[0].Subject.CommonName
	}
	if cn := served(first); cn != "primero" {
		t.Fatalf("certificado servido: %s", cn)
	}

	// Renovación en caliente.
	time.Sleep(10 * time.Millisecond)
	second := writeCert(t, dir, "segundo")
	future := time.Now().Add(time.Second)
	os.Chtimes(filepath.Join(dir, "cert.pem"), future, future)
	if cn := served(second); cn != "segundo" {
		t.Fatalf("tras renovar se sirvió: %s", cn)
	}

	// Un certificado roto en disco no tumba el servicio.
	os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("roto"), 0o600)
	later := future.Add(time.Second)
	os.Chtimes(filepath.Join(dir, "cert.pem"), later, later)
	if cn := served(second); cn != "segundo" {
		t.Fatalf("con el archivo roto se sirvió: %s", cn)
	}

	// TLS 1.1 no se acepta.
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11})
	if err == nil {
		conn.Close()
		t.Fatal("se aceptó TLS 1.1")
	}
}

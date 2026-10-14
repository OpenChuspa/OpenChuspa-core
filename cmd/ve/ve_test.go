package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenChuspa/OpenChuspa-core/internal/auth"
	"github.com/OpenChuspa/OpenChuspa-core/internal/s3api"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage/fsbolt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	testKey    = "parcero"
	testSecret = "0123456789abcdef0123456789abcdef"
)

func TestParseLocation(t *testing.T) {
	tests := []struct {
		in      string
		want    location
		wantErr bool
	}{
		{"chuspa://", location{}, false},
		{"chuspa://feria", location{Bucket: "feria"}, false},
		{"chuspa://feria/", location{Bucket: "feria"}, false},
		{"chuspa://feria/test/a.txt", location{Bucket: "feria", Key: "test/a.txt"}, false},
		{"s3://feria/a", location{Bucket: "feria", Key: "a"}, false},
		{"chuspa:///a", location{}, true},
		{"./a.txt", location{}, true},
	}
	for _, tt := range tests {
		got, err := parseLocation(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseLocation(%q) = %+v, %v", tt.in, got, err)
		}
	}
}

func TestUploadKeyAndConnector(t *testing.T) {
	dst := location{Bucket: "b", Key: "test/"}
	if got := uploadKey(dst, "./dir/file.txt"); got != "test/file.txt" {
		t.Errorf("uploadKey con prefijo = %q", got)
	}
	if got := uploadKey(location{Bucket: "b"}, "file.txt"); got != "file.txt" {
		t.Errorf("uploadKey en la raíz = %q", got)
	}
	if got := uploadKey(location{Bucket: "b", Key: "otro.txt"}, "file.txt"); got != "otro.txt" {
		t.Errorf("uploadKey con nombre explícito = %q", got)
	}
	got := dropConnector([]string{"a.txt", "a", "chuspa://b/"})
	if strings.Join(got, " ") != "a.txt chuspa://b/" {
		t.Errorf("dropConnector = %v", got)
	}
	// Un archivo que se llama "a" no es el conector si no va antes del destino.
	got = dropConnector([]string{"a", "chuspa://b/"})
	if strings.Join(got, " ") != "a chuspa://b/" {
		t.Errorf("dropConnector con archivo llamado a = %v", got)
	}
}

func TestSafeLocalPath(t *testing.T) {
	for _, bad := range []string{"", "../../etc/passwd", "a/../../x", "/etc/passwd", "dir/", "a\x00b"} {
		if _, ok := safeLocalPath("/tmp/x", bad); ok {
			t.Errorf("safeLocalPath aceptó %q", bad)
		}
	}
	if p, ok := safeLocalPath("/tmp/x", "fotos/cali.jpg"); !ok || p != filepath.Join("/tmp/x", "fotos", "cali.jpg") {
		t.Errorf("safeLocalPath = %q, %v", p, ok)
	}
}

func TestPartSize(t *testing.T) {
	if got := partSizeFor(100 << 20); got != minPartSize {
		t.Errorf("100 MiB: parte de %d", got)
	}
	size := int64(400 << 30)
	ps := partSizeFor(size)
	if (size+ps-1)/ps > maxParts || ps%(1<<20) != 0 {
		t.Errorf("400 GiB: parte de %d", ps)
	}
}

func TestLoadConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := loadConfig(env(nil)); err == nil || !strings.Contains(err.Error(), "faltan credenciales") {
		t.Fatalf("sin credenciales: %v", err)
	}
	c, err := loadConfig(env(map[string]string{"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s", "AWS_REGION": "us-east-1"}))
	if err != nil || c.Endpoint != defaultEndpoint || c.Region != "us-east-1" {
		t.Fatalf("config = %+v, %v", c, err)
	}
	if _, err := loadConfig(env(map[string]string{"VE_ENDPOINT": "localhost:9000", "AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s"})); err == nil {
		t.Fatal("se esperaba rechazo de un endpoint sin esquema")
	}
}

// newMotor levanta el motor real en memoria de proceso y devuelve un cliente
// configurado como lo haría ve.
func newMotor(t *testing.T) *s3.Client {
	t.Helper()
	backend, _, err := fsbolt.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	api := &s3api.Server{
		Backend:  backend,
		Region:   defaultRegion,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verifier: &auth.Verifier{AccessKey: testKey, SecretKey: testSecret, Region: defaultRegion},
	}
	srv := httptest.NewServer(api)
	t.Cleanup(func() { srv.Close(); backend.Close() })
	return newClient(config{
		Endpoint: srv.URL,
		Region:   defaultRegion,
		Creds:    aws.Credentials{AccessKeyID: testKey, SecretAccessKey: testSecret},
	})
}

// ve ejecuta un comando y devuelve su salida.
func ve(t *testing.T, c *s3.Client, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	out = &buf
	t.Cleanup(func() { out = os.Stdout })
	err := commands[args[0]](context.Background(), c, args[1:])
	return buf.String(), err
}

func mustVe(t *testing.T, c *s3.Client, args ...string) string {
	t.Helper()
	s, err := ve(t, c, args...)
	if err != nil {
		t.Fatalf("ve %s: %s", strings.Join(args, " "), explain(err, "test"))
	}
	return s
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestComandosDeLaLanding(t *testing.T) {
	c := newMotor(t)
	dir := t.TempDir()
	t.Chdir(dir)
	writeFile(t, "file.txt", []byte("hola parce"))

	mustVe(t, c, "crea", "chuspa://prod-chuspa-name")
	if s := mustVe(t, c, "mira", "chuspa://"); !strings.Contains(s, "chuspa://prod-chuspa-name") {
		t.Fatalf("mira chuspa:// = %q", s)
	}

	mustVe(t, c, "subi", "./file.txt", "a", "chuspa://prod-chuspa-name/test/")
	if s := mustVe(t, c, "mira", "chuspa://prod-chuspa-name/"); !strings.Contains(s, "test/") {
		t.Fatalf("mira de la raíz no muestra la carpeta: %q", s)
	}
	if s := mustVe(t, c, "mira", "chuspa://prod-chuspa-name/", "-r"); !strings.Contains(s, "test/file.txt") {
		t.Fatalf("mira -r = %q", s)
	}

	if err := os.Mkdir("bajadas", 0o755); err != nil {
		t.Fatal(err)
	}
	mustVe(t, c, "baja", "chuspa://prod-chuspa-name/test/file.txt", "bajadas")
	if got, _ := os.ReadFile("bajadas/file.txt"); string(got) != "hola parce" {
		t.Fatalf("contenido bajado = %q", got)
	}

	mustVe(t, c, "borra", "chuspa://prod-chuspa-name/test/file.txt")
	if _, err := ve(t, c, "borra", "chuspa://prod-chuspa-name/test/file.txt"); apiCode(err) != "NotFound" {
		t.Fatalf("borrar dos veces: %v", err)
	}
	mustVe(t, c, "borra", "chuspa://prod-chuspa-name")
	if s := mustVe(t, c, "mira"); s != "" {
		t.Fatalf("quedaron chuspas: %q", s)
	}
}

func TestSincroniza(t *testing.T) {
	c := newMotor(t)
	dir := t.TempDir()
	fotos := filepath.Join(dir, "mis-fotos")
	writeFile(t, filepath.Join(fotos, "a.jpg"), []byte("a"))
	writeFile(t, filepath.Join(fotos, "viaje", "b.jpg"), []byte("bb"))
	mustVe(t, c, "crea", "chuspa://backup-chuspa")

	if s := mustVe(t, c, "sincroniza", fotos, "chuspa://backup-chuspa/"); !strings.Contains(s, "2 subidos, 0 sin cambios") {
		t.Fatalf("primera sincronización: %q", s)
	}
	if s := mustVe(t, c, "sincroniza", fotos, "chuspa://backup-chuspa/"); !strings.Contains(s, "0 subidos, 2 sin cambios") {
		t.Fatalf("segunda sincronización: %q", s)
	}
	writeFile(t, filepath.Join(fotos, "a.jpg"), []byte("cambiada"))
	if s := mustVe(t, c, "sincroniza", fotos, "chuspa://backup-chuspa/"); !strings.Contains(s, "1 subidos, 1 sin cambios") {
		t.Fatalf("tras cambiar un archivo: %q", s)
	}

	// Una key hostil no puede salirse de la carpeta de destino.
	writeFile(t, filepath.Join(dir, "x"), []byte("x"))
	mustVe(t, c, "subi", filepath.Join(dir, "x"), "chuspa://backup-chuspa/../../escapada")

	copia := filepath.Join(dir, "copia")
	if s := mustVe(t, c, "sincroniza", "chuspa://backup-chuspa", copia); !strings.Contains(s, "2 bajados") {
		t.Fatalf("bajada: %q", s)
	}
	if got, _ := os.ReadFile(filepath.Join(copia, "viaje", "b.jpg")); string(got) != "bb" {
		t.Fatalf("contenido sincronizado = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escapada")); err == nil {
		t.Fatal("la key hostil escribió fuera del destino")
	}
	if s := mustVe(t, c, "sincroniza", "chuspa://backup-chuspa", copia); !strings.Contains(s, "0 bajados, 2 sin cambios") {
		t.Fatalf("segunda bajada: %q", s)
	}

	if _, err := ve(t, c, "borra", "chuspa://backup-chuspa"); apiCode(err) != "BucketNotEmpty" {
		t.Fatalf("borrar chuspa llena: %v", err)
	}
	if s := mustVe(t, c, "borra", "chuspa://backup-chuspa", "-r"); !strings.Contains(s, "borrados 3 objetos") {
		t.Fatalf("borra -r = %q", s)
	}
}

func TestSubidaMultipart(t *testing.T) {
	c := newMotor(t)
	dir := t.TempDir()
	data := make([]byte, multipartThreshold+3<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "grande.bin")
	writeFile(t, src, data)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	mustVe(t, c, "crea", "chuspa://feria-de-cali")
	mustVe(t, c, "subi", src, "chuspa://feria-de-cali/")
	head, err := c.HeadObject(context.Background(), &s3.HeadObjectInput{Bucket: aws.String("feria-de-cali"), Key: aws.String("grande.bin")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(aws.ToString(head.ETag), `-5"`) {
		t.Fatalf("se esperaba una subida en 5 partes, ETag %s", aws.ToString(head.ETag))
	}
	dest := filepath.Join(dir, "vuelta.bin")
	mustVe(t, c, "baja", "chuspa://feria-de-cali/grande.bin", dest)
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("el archivo grande no volvió igual (err %v)", err)
	}
}

func TestErroresEntendibles(t *testing.T) {
	c := newMotor(t)
	_, err := ve(t, c, "mira", "chuspa://no-existe/")
	if msg := explain(err, ""); msg != "chuspa://no-existe: esa chuspa no existe" {
		t.Errorf("chuspa inexistente: %q", msg)
	}
	_, err = ve(t, c, "subi", "x.txt")
	if _, ok := err.(usageError); !ok {
		t.Errorf("subi sin destino debería ser error de uso: %v", err)
	}
	_, err = ve(t, c, "borra", "chuspa://b", "--forzado")
	if _, ok := err.(usageError); !ok {
		t.Errorf("opción desconocida debería ser error de uso: %v", err)
	}

	mal := newClient(config{Endpoint: "http://127.0.0.1:1", Region: defaultRegion, Creds: aws.Credentials{AccessKeyID: "k", SecretAccessKey: "s"}},
		func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = mira(ctx, mal, nil)
	if msg := explain(err, "http://127.0.0.1:1"); !strings.Contains(msg, "no encuentro el motor") {
		t.Errorf("motor apagado: %q", msg)
	}
}

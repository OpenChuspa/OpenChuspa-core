package fsbolt

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

func read(t *testing.T, b *Backend, bucket, key string) (storage.Object, string) {
	t.Helper()
	obj, f, err := b.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("GetObject(%s, %q): %v", bucket, key, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return obj, string(data)
}

func verify(t *testing.T, b *Backend) []Problem {
	t.Helper()
	var problems []Problem
	if _, err := b.Verify(ctx, func(p Problem) { problems = append(problems, p) }); err != nil {
		t.Fatal(err)
	}
	return problems
}

// testdata/formato1 lo escribió chuspa v0.2.0 con el AWS CLI: las chuspas
// feria-de-cali y vacia, los objetos test/salsa.txt (con metadatos) y
// ../../etc/passwd (con checksum SHA256), y una subida multipart en curso
// con una parte.
func TestUpgradeDesdeFormato1(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.CopyFS(dir, os.DirFS("testdata/formato1")); err != nil {
		t.Fatal(err)
	}
	b := open(t, dir)

	obj, data := read(t, b, "feria-de-cali", "test/salsa.txt")
	if data != "hola parce" || obj.ContentType != "text/plain" || obj.User["autor"] != "parchao" {
		t.Fatalf("objeto migrado: %q %+v", data, obj.Metadata)
	}
	if _, data := read(t, b, "feria-de-cali", "../../etc/passwd"); data != "otra" {
		t.Fatalf("key hostil migrada: %q", data)
	}
	buckets, err := b.ListBuckets(ctx)
	if err != nil || len(buckets) != 2 {
		t.Fatalf("chuspas tras migrar: %v %v", buckets, err)
	}
	if ups, err := b.ListUploads(ctx, "feria-de-cali"); err != nil || len(ups) != 1 {
		t.Fatalf("la subida en curso no sobrevivió: %v %v", ups, err)
	}
	if p := verify(t, b); len(p) != 0 {
		t.Fatalf("problemas tras migrar: %+v", p)
	}
	for _, name := range []string{"feria-de-cali", "vacia"} {
		if _, err := os.Stat(filepath.Join(dir, markersDir, name)); err != nil {
			t.Fatalf("falta la marca de %s: %v", name, err)
		}
	}

	// Tras migrar, el índice se puede reconstruir desde cero.
	b.Close()
	if err := os.Remove(filepath.Join(dir, "meta.db")); err != nil {
		t.Fatal(err)
	}
	res, err := Rebuild(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Buckets != 2 || res.Objects != 2 || res.Recovered != 1 {
		t.Fatalf("reconstrucción: %+v", res)
	}
	b = open(t, dir)
	if _, data := read(t, b, "feria-de-cali", "test/salsa.txt"); data != "hola parce" {
		t.Fatalf("tras reconstruir: %q", data)
	}
}

func TestFormatoMasNuevoNoSeAbre(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	if err := b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktMeta).Put(keyFormat, []byte("99"))
	}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	if _, _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "versión más nueva") {
		t.Fatalf("se esperaba rechazo del formato 99: %v", err)
	}
	if _, err := OpenReadOnly(dir); err == nil {
		t.Fatal("inspect no debería abrir un formato más nuevo")
	}
}

func TestRebuild(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	for _, name := range []string{"feria", "vacia"} {
		if err := b.CreateBucket(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	put(t, b, "feria", "a.txt", "primera")
	time.Sleep(10 * time.Millisecond)
	put(t, b, "feria", "a.txt", "segunda")
	put(t, b, "feria", "../../x", "hostil")
	id, err := b.CreateUpload(ctx, storage.Upload{Bucket: "feria", Key: "grande"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := b.Stage(ctx, strings.NewReader("parte"))
	if _, err := b.PutPart(ctx, "feria", "grande", id, 1, s, storage.PutInput{ETag: "e"}); err != nil {
		t.Fatal(err)
	}
	// Una versión vieja que quedó en disco por una caída entre el índice y el
	// borrado: la reconstrucción debe preferir la más nueva.
	old := objectRecord{Blob: "00000000000000000000000000000000", Object: storage.Object{Size: 4, Modified: time.Unix(0, 0)}}
	writeFile(t, b.blobPath(old.Blob), "vieja")
	old.Size = 5
	if err := b.writeSidecar("feria", "a.txt", old); err != nil {
		t.Fatal(err)
	}
	b.Close()

	// Índice dañado.
	if err := os.WriteFile(filepath.Join(dir, "meta.db"), []byte("basura"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(dir); err == nil {
		t.Fatal("se esperaba que el índice dañado no abriera")
	}
	res, err := Rebuild(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Buckets != 2 || res.Objects != 2 || res.Recovered != 2 || res.OldIndex == "" {
		t.Fatalf("reconstrucción: %+v", res)
	}
	if got, _ := os.ReadFile(res.OldIndex); string(got) != "basura" {
		t.Fatal("el índice anterior no se conservó")
	}

	b = open(t, dir)
	if _, data := read(t, b, "feria", "a.txt"); data != "segunda" {
		t.Fatalf("a.txt = %q", data)
	}
	if _, data := read(t, b, "feria", "../../x"); data != "hostil" {
		t.Fatalf("key hostil = %q", data)
	}
	if _, err := b.GetBucket(ctx, "vacia"); err != nil {
		t.Fatalf("la chuspa vacía no se recuperó: %v", err)
	}
	if p := verify(t, b); len(p) != 0 {
		t.Fatalf("problemas tras reconstruir: %+v", p)
	}
	if n := countFiles(t, filepath.Join(dir, recoveredDir)); n != 2 {
		t.Fatalf("recuperados = %d, se esperaban 2", n)
	}
}

func TestRebuildConServidorAbierto(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)
	if _, err := Rebuild(dir); err == nil || !strings.Contains(err.Error(), "en uso") {
		t.Fatalf("se esperaba rechazo con el índice abierto: %v", err)
	}
}

func TestVerifyDetectaDanos(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	if err := b.CreateBucket(ctx, "feria"); err != nil {
		t.Fatal(err)
	}
	put(t, b, "feria", "alterado", "contenido original")
	put(t, b, "feria", "truncado", "contenido original")
	put(t, b, "feria", "sin-meta", "contenido original")
	put(t, b, "feria", "sano", "contenido original")

	rec := func(key string) objectRecord {
		r, err := b.getRecord("feria", key)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	writeFile(t, b.blobPath(rec("alterado").Blob), "contenido 0riginal")
	writeFile(t, b.blobPath(rec("truncado").Blob), "contenido")
	if err := os.Remove(b.sidecarPath(rec("sin-meta").Blob)); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, p := range verify(t, b) {
		got[p.Key] = p.Reason
	}
	if len(got) != 3 || !strings.Contains(got["alterado"], "hash") || !strings.Contains(got["truncado"], "tamaño") ||
		!strings.Contains(got["sin-meta"], "metadatos") {
		t.Fatalf("problemas = %v", got)
	}
}

func TestDiscoLleno(t *testing.T) {
	b := open(t, t.TempDir())
	b.MinFreeBytes = math.MaxInt64
	if _, err := b.Stage(ctx, strings.NewReader("x")); !errors.Is(err, storage.ErrInsufficientStorage) {
		t.Fatalf("se esperaba ErrInsufficientStorage: %v", err)
	}
	b.MinFreeBytes = 1
	if _, err := b.Stage(ctx, strings.NewReader("x")); err != nil {
		t.Fatalf("con espacio de sobra: %v", err)
	}
	if free, err := FreeBytes(b.dir); err != nil || free == 0 {
		t.Fatalf("FreeBytes = %d, %v", free, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

package fsbolt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

var ctx = context.Background()

func open(t *testing.T, dir string) *Backend {
	t.Helper()
	b, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func put(t *testing.T, b *Backend, bucket, key, content string) {
	t.Helper()
	s, err := b.Stage(ctx, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.PutObject(ctx, bucket, key, s, storage.PutInput{ETag: "etag"}); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, b *Backend, bucket, key string) string {
	t.Helper()
	_, rc, err := b.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	return string(data)
}

// countFiles cuenta los archivos de contenido; los metadatos que acompañan a
// cada blob no cuentan.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasSuffix(d.Name(), sidecarSuffix) {
			n++
		}
		return nil
	})
	return n
}

func TestBuckets(t *testing.T) {
	b := open(t, t.TempDir())
	if err := b.CreateBucket(ctx, "feria-de-cali"); err != nil {
		t.Fatal(err)
	}
	if err := b.CreateBucket(ctx, "feria-de-cali"); !errors.Is(err, storage.ErrBucketExists) {
		t.Fatalf("se esperaba ErrBucketExists, se obtuvo %v", err)
	}
	put(t, b, "feria-de-cali", "a", "x")
	if err := b.DeleteBucket(ctx, "feria-de-cali"); !errors.Is(err, storage.ErrBucketNotEmpty) {
		t.Fatalf("se esperaba ErrBucketNotEmpty, se obtuvo %v", err)
	}
	if err := b.DeleteObject(ctx, "feria-de-cali", "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteBucket(ctx, "feria-de-cali"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetBucket(ctx, "feria-de-cali"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("se esperaba ErrBucketNotFound, se obtuvo %v", err)
	}
}

// Las keys hostiles son nombres normales: nada sale de la carpeta de datos.
func TestHostileKeysStayInsideDataDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	b := open(t, dir)
	b.CreateBucket(ctx, "chuspa")
	keys := []string{"../../etc/passwd", "/etc/passwd", "a", "a/b", "a/b/", "..", ".", "con\x00nulo", strings.Repeat("k", 1024), "ñ/😀"}
	for _, k := range keys {
		put(t, b, "chuspa", k, "contenido de "+k)
	}
	for _, k := range keys {
		if got := get(t, b, "chuspa", k); got != "contenido de "+k {
			t.Errorf("key %q: contenido %q", k, got)
		}
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("aparecieron archivos fuera de la carpeta de datos: %v", entries)
	}
	if n := countFiles(t, filepath.Join(dir, "blobs")); n != len(keys) {
		t.Fatalf("se esperaban %d blobs, hay %d", len(keys), n)
	}
}

func TestOverwriteAndDeleteRemoveBlobs(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	b.CreateBucket(ctx, "chuspa")
	put(t, b, "chuspa", "k", "uno")
	put(t, b, "chuspa", "k", "dos")
	if got := get(t, b, "chuspa", "k"); got != "dos" {
		t.Fatalf("contenido = %q", got)
	}
	if n := countFiles(t, filepath.Join(dir, "blobs")); n != 1 {
		t.Fatalf("tras sobrescribir quedan %d blobs", n)
	}
	b.DeleteObject(ctx, "chuspa", "k", "")
	if n := countFiles(t, filepath.Join(dir, "blobs")); n != 0 {
		t.Fatalf("tras borrar quedan %d blobs", n)
	}
	if _, _, err := b.GetObject(ctx, "chuspa", "k"); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("se esperaba ErrObjectNotFound, se obtuvo %v", err)
	}
}

func TestPersistenceAndOrphans(t *testing.T) {
	dir := t.TempDir()
	b, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b.CreateBucket(ctx, "chuspa")
	put(t, b, "chuspa", "k", "sobrevive")
	// Simula una caída: una escritura a medias y un blob publicado sin índice.
	if _, err := b.Stage(ctx, strings.NewReader("a medias")); err != nil {
		t.Fatal(err)
	}
	s, _ := b.Stage(ctx, strings.NewReader("sin índice"))
	if _, err := b.publish(s); err != nil {
		t.Fatal(err)
	}
	b.Close()

	b2, orphans, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if orphans != 2 {
		t.Fatalf("se esperaban 2 huérfanos, se borraron %d", orphans)
	}
	if got := get(t, b2, "chuspa", "k"); got != "sobrevive" {
		t.Fatalf("contenido tras reabrir = %q", got)
	}
	if n := countFiles(t, filepath.Join(dir, "blobs")) + countFiles(t, filepath.Join(dir, "staging")); n != 1 {
		t.Fatalf("quedan %d archivos, se esperaba 1", n)
	}
}

func TestListObjects(t *testing.T) {
	b := open(t, t.TempDir())
	b.CreateBucket(ctx, "chuspa")
	b.CreateBucket(ctx, "chuspa2")
	put(t, b, "chuspa2", "intruso", "x")
	for _, k := range []string{"a.txt", "fotos/2025/a.jpg", "fotos/2025/b.jpg", "fotos/2026/c.jpg", "fotos/z.jpg", "música/salsa.mp3", "z.txt"} {
		put(t, b, "chuspa", k, "x")
	}

	out, err := b.ListObjects(ctx, "chuspa", storage.ListInput{Delimiter: "/", MaxKeys: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(out); got != "a.txt z.txt | fotos/ música/" {
		t.Fatalf("raíz con delimitador: %s", got)
	}
	out, _ = b.ListObjects(ctx, "chuspa", storage.ListInput{Prefix: "fotos/", Delimiter: "/", MaxKeys: 1000})
	if got := keys(out); got != "fotos/z.jpg | fotos/2025/ fotos/2026/" {
		t.Fatalf("prefijo con delimitador: %s", got)
	}
	out, _ = b.ListObjects(ctx, "chuspa", storage.ListInput{After: "fotos/2026/c.jpg", MaxKeys: 1000})
	if got := keys(out); got != "fotos/z.jpg música/salsa.mp3 z.txt | " {
		t.Fatalf("start-after: %s", got)
	}

	// Paginación de a un elemento: debe recorrer todo sin repetir ni saltar.
	var seen []string
	in := storage.ListInput{Delimiter: "/", MaxKeys: 1}
	for i := 0; i < 10; i++ {
		out, _ := b.ListObjects(ctx, "chuspa", in)
		for _, o := range out.Objects {
			seen = append(seen, o.Key)
		}
		seen = append(seen, out.CommonPrefixes...)
		if !out.Truncated {
			break
		}
		in.After, in.AfterPrefix = out.Next, out.NextIsPrefix
	}
	if got := strings.Join(seen, " "); got != "a.txt fotos/ música/ z.txt" {
		t.Fatalf("paginación: %s", got)
	}
}

func keys(out storage.ListOutput) string {
	var ks []string
	for _, o := range out.Objects {
		ks = append(ks, o.Key)
	}
	return strings.Join(ks, " ") + " | " + strings.Join(out.CommonPrefixes, " ")
}

func TestListPagination(t *testing.T) {
	b := open(t, t.TempDir())
	b.CreateBucket(ctx, "chuspa")
	const total = 250
	for i := 0; i < total; i++ {
		s, _ := b.Stage(ctx, strings.NewReader(""))
		if _, err := b.PutObject(ctx, "chuspa", fmt.Sprintf("obj-%05d", i), s, storage.PutInput{}); err != nil {
			t.Fatal(err)
		}
	}
	in := storage.ListInput{MaxKeys: 100}
	count, pages, last := 0, 0, ""
	for {
		out, err := b.ListObjects(ctx, "chuspa", in)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, o := range out.Objects {
			if o.Key <= last {
				t.Fatalf("orden roto: %q después de %q", o.Key, last)
			}
			last = o.Key
			count++
		}
		if !out.Truncated {
			break
		}
		in.After = out.Next
	}
	if count != total || pages != 3 {
		t.Fatalf("se listaron %d objetos en %d páginas", count, pages)
	}
}

func TestMultipart(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	b.CreateBucket(ctx, "chuspa")
	id, err := b.CreateUpload(ctx, storage.Upload{Bucket: "chuspa", Key: "grande"})
	if err != nil {
		t.Fatal(err)
	}
	part1 := bytes.Repeat([]byte("a"), storage.MinPartSize)
	part2 := []byte("final")
	etags := []string{"0cc175b9c0f1b6a831c399e269772661", "92eb5ffee6ae2fec3ad71c777531578f"}
	for i, data := range [][]byte{part1, part2} {
		s, _ := b.Stage(ctx, bytes.NewReader(data))
		if _, err := b.PutPart(ctx, "chuspa", "grande", id, i+1, s, storage.PutInput{ETag: etags[i]}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.PutPart(ctx, "chuspa", "otra-key", id, 3, mustStage(t, b, "x"), storage.PutInput{}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("parte con otra key: %v", err)
	}

	done := []storage.CompletedPart{{Number: 1, ETag: etags[0]}, {Number: 2, ETag: etags[1]}}
	if _, err := b.CompleteUpload(ctx, "chuspa", "grande", id, []storage.CompletedPart{done[1], done[0]}, "", storage.Precondition{}); !errors.Is(err, storage.ErrPartOrder) {
		t.Fatalf("orden: %v", err)
	}
	if _, err := b.CompleteUpload(ctx, "chuspa", "grande", id, []storage.CompletedPart{{Number: 1, ETag: "otro"}}, "", storage.Precondition{}); !errors.Is(err, storage.ErrInvalidPart) {
		t.Fatalf("etag: %v", err)
	}
	if _, err := b.CompleteUpload(ctx, "chuspa", "grande", id, []storage.CompletedPart{done[0], {Number: 3, ETag: "x"}}, "", storage.Precondition{}); !errors.Is(err, storage.ErrInvalidPart) {
		t.Fatalf("parte inexistente: %v", err)
	}
	b.PutPart(ctx, "chuspa", "grande", id, 3, mustStage(t, b, "x"), storage.PutInput{ETag: etags[1]})
	small := []storage.CompletedPart{done[1], {Number: 3, ETag: etags[1]}}
	if _, err := b.CompleteUpload(ctx, "chuspa", "grande", id, small, "", storage.Precondition{}); !errors.Is(err, storage.ErrPartTooSmall) {
		t.Fatalf("parte intermedia pequeña: %v", err)
	}

	obj, err := b.CompleteUpload(ctx, "chuspa", "grande", id, done, "", storage.Precondition{})
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size != int64(len(part1)+len(part2)) || !strings.HasSuffix(obj.ETag, "-2") {
		t.Fatalf("objeto = %+v", obj)
	}
	if got := get(t, b, "chuspa", "grande"); got != string(part1)+string(part2) {
		t.Fatal("el contenido unido no coincide")
	}
	if n := countFiles(t, filepath.Join(dir, "blobs")); n != 1 {
		t.Fatalf("tras completar quedan %d blobs", n)
	}
	// Reintentar la misma subida devuelve el mismo objeto; otra cualquiera no existe.
	if again, err := b.CompleteUpload(ctx, "chuspa", "grande", id, done, "", storage.Precondition{}); err != nil || again.ETag != obj.ETag {
		t.Fatalf("completar dos veces: %+v, %v", again, err)
	}
	if _, err := b.CompleteUpload(ctx, "chuspa", "grande", "no-existe", done, "", storage.Precondition{}); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("subida inexistente: %v", err)
	}

	id2, _ := b.CreateUpload(ctx, storage.Upload{Bucket: "chuspa", Key: "abortada"})
	b.PutPart(ctx, "chuspa", "abortada", id2, 1, mustStage(t, b, "x"), storage.PutInput{})
	if err := b.AbortUpload(ctx, "chuspa", "abortada", id2); err != nil {
		t.Fatal(err)
	}
	if n := countFiles(t, filepath.Join(dir, "blobs")); n != 1 {
		t.Fatalf("tras abortar quedan %d blobs", n)
	}
}

func mustStage(t *testing.T, b *Backend, content string) storage.Staged {
	t.Helper()
	s, err := b.Stage(ctx, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSweep(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	b.CreateBucket(ctx, "chuspa")
	put(t, b, "chuspa", "k", "vivo")
	orphan, _ := b.publish(mustStage(t, b, "huérfano"))
	mustStage(t, b, "a medias")
	old, _ := b.CreateUpload(ctx, storage.Upload{Bucket: "chuspa", Key: "vieja"})
	b.PutPart(ctx, "chuspa", "vieja", old, 1, mustStage(t, b, "x"), storage.PutInput{})
	now := time.Now()

	// Dentro del margen no se toca nada: podrían ser escrituras en curso.
	res, err := b.Sweep(now, time.Hour, time.Hour, 24*time.Hour)
	if err != nil || res.Orphans != 0 || res.ExpiredUploads != 0 {
		t.Fatalf("pasada dentro del margen: %+v, %v", res, err)
	}
	if _, err := os.Stat(b.blobPath(orphan.id)); err != nil {
		t.Fatal("se borró un blob reciente")
	}

	// Vencido el margen: se van el huérfano, el archivo de staging y la
	// subida abandonada con su parte.
	res, err = b.Sweep(now.Add(48*time.Hour), time.Hour, time.Hour, 24*time.Hour)
	if err != nil || res.Orphans != 2 || res.ExpiredUploads != 1 {
		t.Fatalf("pasada vencida: %+v, %v", res, err)
	}
	if got := get(t, b, "chuspa", "k"); got != "vivo" {
		t.Fatalf("se dañó un objeto vivo: %q", got)
	}
	if n := countFiles(t, filepath.Join(dir, "blobs")) + countFiles(t, filepath.Join(dir, "staging")); n != 1 {
		t.Fatalf("quedan %d archivos, se esperaba 1", n)
	}
	if _, err := b.ListParts(ctx, "chuspa", "vieja", old); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("la subida abandonada sigue viva: %v", err)
	}
}

func TestBackupAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	b, _, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b.CreateBucket(ctx, "chuspa")
	put(t, b, "chuspa", "k", "contenido")
	if err := b.Backup(); err != nil {
		t.Fatal(err)
	}
	b.Close()

	// La copia es un índice completo: sirve para reemplazar meta.db.
	if err := os.Rename(filepath.Join(dir, "meta.db.bak"), filepath.Join(dir, "meta.db")); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got := get(t, ro, "chuspa", "k"); got != "contenido" {
		t.Fatalf("lectura desde la copia del índice: %q", got)
	}
}

func TestPreconditions(t *testing.T) {
	b := open(t, t.TempDir())
	b.CreateBucket(ctx, "chuspa")
	try := func(content string, cond storage.Precondition) error {
		_, err := b.PutObject(ctx, "chuspa", "k", mustStage(t, b, content), storage.PutInput{ETag: "etag-" + content, Cond: cond})
		return err
	}
	if err := try("a", storage.Precondition{IfMatch: "*"}); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("If-Match sobre un objeto inexistente: %v", err)
	}
	if err := try("a", storage.Precondition{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}
	if err := try("b", storage.Precondition{IfNoneMatch: true}); !errors.Is(err, storage.ErrPrecondition) {
		t.Fatalf("If-None-Match sobre un objeto existente: %v", err)
	}
	if err := try("b", storage.Precondition{IfMatch: "otro"}); !errors.Is(err, storage.ErrPrecondition) {
		t.Fatalf("If-Match con otro ETag: %v", err)
	}
	if got := get(t, b, "chuspa", "k"); got != "a" {
		t.Fatalf("una escritura rechazada cambió el objeto: %q", got)
	}
	if err := try("b", storage.Precondition{IfMatch: "etag-a"}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteObject(ctx, "chuspa", "k", "etag-a"); !errors.Is(err, storage.ErrPrecondition) {
		t.Fatalf("borrado con ETag viejo: %v", err)
	}
	if err := b.DeleteObject(ctx, "chuspa", "k", "etag-b"); err != nil {
		t.Fatal(err)
	}
	if n := countFiles(t, filepath.Join(b.dir, "blobs")) + countFiles(t, filepath.Join(b.dir, "staging")); n != 0 {
		t.Fatalf("las escrituras rechazadas dejaron %d archivos", n)
	}
}

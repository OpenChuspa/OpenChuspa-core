// Package fsbolt implementa storage.Backend con blobs opacos en el sistema de
// archivos y un índice bbolt.
//
//	<dir>/meta.db                índice: chuspas, objetos, subidas multipart
//	<dir>/blobs/ab/cd/<id>       contenido, con un nombre generado por el servidor
//	<dir>/blobs/ab/cd/<id>.meta  chuspa, key y registro del objeto (ver Rebuild)
//	<dir>/chuspas/<nombre>       una marca por chuspa
//	<dir>/staging/<id>           escrituras en curso
//
// La key del objeto vive solo en el índice y nunca forma parte de una ruta.
// Toda escritura va a staging, se sincroniza, se mueve a blobs y solo
// entonces se confirma en el índice: una caída deja el objeto anterior o el
// nuevo, nunca uno a medias.
package fsbolt

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/OpenChuspa/OpenChuspa-core/internal/checksum"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

var (
	bktBuckets = []byte("buckets")
	bktObjects = []byte("objects")
	bktUploads = []byte("uploads")
	bktParts   = []byte("parts")
)

// Backend es la implementación en disco local.
type Backend struct {
	dir string
	db  *bolt.DB
	// MinFreeBytes es el espacio libre mínimo en disco para aceptar
	// escrituras; en cero no se comprueba.
	MinFreeBytes int64
}

type objectRecord struct {
	storage.Object
	Blob string `json:"blob"`
	// SHA256 del contenido, calculado por el servidor al escribir. Vacío en
	// los objetos del formato 1.
	SHA256 string `json:"sha256,omitempty"`
}

type partRecord struct {
	storage.Part
	Blob string `json:"blob"`
}

type staged struct {
	b      *Backend
	id     string
	path   string
	size   int64
	sha256 string
}

func (s *staged) Size() int64 { return s.size }
func (s *staged) Discard()    { os.Remove(s.path) }

// Open abre (o crea) el almacenamiento en dir y limpia lo que haya quedado de
// una ejecución anterior: escrituras a medias y blobs sin entrada en el índice.
// Devuelve cuántos huérfanos borró.
//
// Los datos de versiones anteriores se migran al formato actual; los de una
// versión más nueva no se abren.
func Open(dir string) (*Backend, int, error) { return openDir(dir, true) }

// openDir abre el almacenamiento; con sweep borra lo que no esté en el índice.
func openDir(dir string, sweep bool) (*Backend, int, error) {
	for _, d := range []string{dir, filepath.Join(dir, "blobs"), filepath.Join(dir, "staging"), filepath.Join(dir, markersDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, 0, fmt.Errorf("fsbolt: %w", err)
		}
	}
	db, err := bolt.Open(filepath.Join(dir, "meta.db"), 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, 0, fmt.Errorf("fsbolt: no se pudo abrir el índice (¿otro proceso usa %s?): %w", dir, err)
	}
	b := &Backend{dir: dir, db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bktMeta, bktBuckets, bktObjects, bktUploads, bktParts} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = b.checkFormat(false)
	}
	if err == nil && !sweep {
		return b, 0, nil
	}
	if err == nil {
		// Nadie más escribe todavía: todo lo que no esté en el índice sobra.
		var orphans int
		if orphans, err = b.collectOrphans(time.Now(), 0, 0); err == nil {
			return b, orphans, nil
		}
	}
	db.Close()
	return nil, 0, fmt.Errorf("fsbolt: %w", err)
}

// OpenReadOnly abre un almacenamiento existente sin modificarlo, para
// inspeccionarlo. Requiere que ningún servidor lo tenga abierto.
func OpenReadOnly(dir string) (*Backend, error) {
	db, err := bolt.Open(filepath.Join(dir, "meta.db"), 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("fsbolt: no se pudo abrir el índice de %s (¿hay un servidor usándolo?): %w", dir, err)
	}
	b := &Backend{dir: dir, db: db}
	if err := b.checkFormat(true); err != nil {
		db.Close()
		return nil, fmt.Errorf("fsbolt: %w", err)
	}
	return b, nil
}

func (b *Backend) Close() error { return b.db.Close() }

// SweepResult resume una pasada de mantenimiento.
type SweepResult struct {
	Orphans        int // archivos sin referencia borrados
	ExpiredUploads int // subidas multipart abandonadas abortadas
}

// Sweep hace el mantenimiento periódico con el servidor atendiendo
// peticiones: aborta las subidas multipart iniciadas hace más de uploadTTL y
// borra los archivos sin referencia en el índice.
//
// Una escritura en curso tiene su archivo en disco antes de aparecer en el
// índice, así que solo se consideran huérfanos los blobs con más de blobGrace
// de antigüedad y los archivos de staging sin actividad desde hace stagingGrace.
func (b *Backend) Sweep(now time.Time, blobGrace, stagingGrace, uploadTTL time.Duration) (SweepResult, error) {
	var res SweepResult
	var blobs []string
	err := b.db.Update(func(tx *bolt.Tx) error {
		var expired []string
		err := tx.Bucket(bktUploads).ForEach(func(k, v []byte) error {
			var u storage.Upload
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			if uploadTTL > 0 && now.Sub(u.Initiated) > uploadTTL {
				expired = append(expired, string(k))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, id := range expired {
			removed, err := deleteUpload(tx, id)
			if err != nil {
				return err
			}
			blobs = append(blobs, removed...)
		}
		res.ExpiredUploads = len(expired)
		return nil
	})
	if err != nil {
		return res, err
	}
	b.removeBlobs(blobs...)
	res.Orphans, err = b.collectOrphans(now, blobGrace, stagingGrace)
	return res, err
}

// Backup deja una copia consistente del índice en meta.db.bak.
func (b *Backend) Backup() error {
	dst := filepath.Join(b.dir, "meta.db.bak")
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = b.db.View(func(tx *bolt.Tx) error {
		_, err := tx.WriteTo(f)
		return err
	})
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// collectOrphans borra los archivos de staging y los blobs que no tienen
// entrada en el índice y superan la antigüedad indicada. Los candidatos se
// listan antes de leer el índice: un blob antiguo que no aparezca en una
// lectura posterior del índice ya no puede llegar a aparecer.
func (b *Backend) collectOrphans(now time.Time, blobGrace, stagingGrace time.Duration) (int, error) {
	older := func(d fs.DirEntry, grace time.Duration) bool {
		info, err := d.Info()
		return err == nil && now.Sub(info.ModTime()) >= grace
	}
	staging := filepath.Join(b.dir, "staging")
	entries, err := os.ReadDir(staging)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if older(e, stagingGrace) && os.Remove(filepath.Join(staging, e.Name())) == nil {
			removed++
		}
	}

	candidates := map[string][]string{} // id -> el blob y sus metadatos
	err = filepath.WalkDir(filepath.Join(b.dir, "blobs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !older(d, blobGrace) {
			return nil
		}
		if strings.HasSuffix(d.Name(), tmpSuffix) {
			// Metadatos a medio escribir: nunca son la versión vigente.
			if os.Remove(path) == nil {
				removed++
			}
			return nil
		}
		id := blobID(d.Name())
		candidates[id] = append(candidates[id], path)
		return nil
	})
	if err != nil || len(candidates) == 0 {
		return removed, err
	}

	err = b.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bktObjects).ForEach(func(_, v []byte) error {
			var rec objectRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			delete(candidates, rec.Blob)
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket(bktParts).ForEach(func(_, v []byte) error {
			var rec partRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			delete(candidates, rec.Blob)
			return nil
		})
	})
	if err != nil {
		return removed, err
	}
	for _, paths := range candidates {
		for _, path := range paths {
			if os.Remove(path) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// ---- chuspas ----

func (b *Backend) CreateBucket(_ context.Context, name string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		bk := tx.Bucket(bktBuckets)
		if bk.Get([]byte(name)) != nil {
			return storage.ErrBucketExists
		}
		bucket := storage.Bucket{Name: name, Created: time.Now().UTC()}
		if err := b.writeMarker(bucket); err != nil {
			return err
		}
		v, _ := json.Marshal(bucket)
		return bk.Put([]byte(name), v)
	})
}

func (b *Backend) GetBucket(_ context.Context, name string) (storage.Bucket, error) {
	var out storage.Bucket
	err := b.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bktBuckets).Get([]byte(name))
		if v == nil {
			return storage.ErrBucketNotFound
		}
		return json.Unmarshal(v, &out)
	})
	return out, err
}

func (b *Backend) ListBuckets(_ context.Context) ([]storage.Bucket, error) {
	var out []storage.Bucket
	err := b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bktBuckets).ForEach(func(_, v []byte) error {
			var bk storage.Bucket
			if err := json.Unmarshal(v, &bk); err != nil {
				return err
			}
			out = append(out, bk)
			return nil
		})
	})
	return out, err
}

func (b *Backend) DeleteBucket(_ context.Context, name string) error {
	var blobs []string
	err := b.db.Update(func(tx *bolt.Tx) error {
		bk := tx.Bucket(bktBuckets)
		if bk.Get([]byte(name)) == nil {
			return storage.ErrBucketNotFound
		}
		prefix := objectKey(name, "")
		if k, _ := tx.Bucket(bktObjects).Cursor().Seek(prefix); k != nil && bytes.HasPrefix(k, prefix) {
			return storage.ErrBucketNotEmpty
		}
		// Las subidas multipart sin completar se van con la chuspa.
		var ids []string
		err := tx.Bucket(bktUploads).ForEach(func(k, v []byte) error {
			var u storage.Upload
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			if u.Bucket == name {
				ids = append(ids, string(k))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, id := range ids {
			removed, err := deleteUpload(tx, id)
			if err != nil {
				return err
			}
			blobs = append(blobs, removed...)
		}
		return bk.Delete([]byte(name))
	})
	if err == nil {
		b.removeMarker(name)
		b.removeBlobs(blobs...)
	}
	return err
}

// ---- contenido ----

func (b *Backend) Stage(ctx context.Context, r io.Reader) (storage.Staged, error) {
	if err := b.checkSpace(); err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(b.dir, "staging", id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), r)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return nil, diskFull(err)
	}
	return &staged{b: b, id: id, path: path, size: n, sha256: hex.EncodeToString(sum.Sum(nil))}, nil
}

// publish mueve el contenido de staging a su ubicación definitiva.
func (b *Backend) publish(s storage.Staged) (*staged, error) {
	st, ok := s.(*staged)
	if !ok || st.b != b {
		return nil, errors.New("fsbolt: contenido preparado por otro backend")
	}
	dst := b.blobPath(st.id)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Rename(st.path, dst); err != nil {
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		os.Remove(dst)
		return nil, err
	}
	return st, nil
}

func (b *Backend) blobPath(id string) string {
	return filepath.Join(b.dir, "blobs", id[:2], id[2:4], id)
}

func (b *Backend) removeBlobs(ids ...string) {
	for _, id := range ids {
		if id != "" {
			os.Remove(b.blobPath(id))
			os.Remove(b.sidecarPath(id))
		}
	}
}

// ---- objetos ----

func (b *Backend) PutObject(_ context.Context, bucket, key string, s storage.Staged, in storage.PutInput) (storage.Object, error) {
	st, err := b.publish(s)
	if err != nil {
		s.Discard()
		return storage.Object{}, err
	}
	rec := objectRecord{Blob: st.id, SHA256: st.sha256, Object: storage.Object{
		Key: key, Size: st.size, ETag: in.ETag, Modified: time.Now().UTC(),
		Metadata: in.Metadata, ChecksumAlgo: in.ChecksumAlgo, Checksum: in.Checksum,
	}}
	if err := b.writeSidecar(bucket, key, rec); err != nil {
		b.removeBlobs(st.id)
		return storage.Object{}, diskFull(err)
	}
	var old string
	err = b.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bktBuckets).Get([]byte(bucket)) == nil {
			return storage.ErrBucketNotFound
		}
		var err error
		old, err = putObject(tx, bucket, key, rec, in.Cond)
		return err
	})
	if err != nil {
		b.removeBlobs(st.id)
		return storage.Object{}, err
	}
	b.removeBlobs(old)
	return rec.Object, nil
}

// checkPrecondition evalúa cond contra el registro actual (nil si no existe).
func checkPrecondition(prev *objectRecord, cond storage.Precondition) error {
	if cond.IfNoneMatch && prev != nil {
		return storage.ErrPrecondition
	}
	if cond.IfMatch != "" {
		if prev == nil {
			return storage.ErrObjectNotFound
		}
		if cond.IfMatch != "*" && cond.IfMatch != prev.ETag {
			return storage.ErrPrecondition
		}
	}
	return nil
}

// putObject escribe el registro y devuelve el blob que reemplaza, si lo hay.
func putObject(tx *bolt.Tx, bucket, key string, rec objectRecord, cond storage.Precondition) (string, error) {
	objects := tx.Bucket(bktObjects)
	k := objectKey(bucket, key)
	var old string
	var prev *objectRecord
	if v := objects.Get(k); v != nil {
		prev = new(objectRecord)
		if err := json.Unmarshal(v, prev); err != nil {
			return "", err
		}
		old = prev.Blob
	}
	if err := checkPrecondition(prev, cond); err != nil {
		return "", err
	}
	v, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	return old, objects.Put(k, v)
}

func (b *Backend) getRecord(bucket, key string) (objectRecord, error) {
	var rec objectRecord
	err := b.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bktBuckets).Get([]byte(bucket)) == nil {
			return storage.ErrBucketNotFound
		}
		v := tx.Bucket(bktObjects).Get(objectKey(bucket, key))
		if v == nil {
			return storage.ErrObjectNotFound
		}
		return json.Unmarshal(v, &rec)
	})
	rec.Key = key
	return rec, err
}

func (b *Backend) HeadObject(_ context.Context, bucket, key string) (storage.Object, error) {
	rec, err := b.getRecord(bucket, key)
	return rec.Object, err
}

func (b *Backend) GetObject(_ context.Context, bucket, key string) (storage.Object, io.ReadSeekCloser, error) {
	// Si el objeto se reemplaza entre leer el índice y abrir el blob, el
	// blob viejo ya no existe: se vuelve a leer el índice.
	for attempt := 0; ; attempt++ {
		rec, err := b.getRecord(bucket, key)
		if err != nil {
			return storage.Object{}, nil, err
		}
		f, err := os.Open(b.blobPath(rec.Blob))
		if err == nil {
			return rec.Object, f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) || attempt == 2 {
			return storage.Object{}, nil, fmt.Errorf("fsbolt: blob del objeto: %w", err)
		}
	}
}

func (b *Backend) DeleteObject(_ context.Context, bucket, key, ifMatch string) error {
	var old string
	err := b.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bktBuckets).Get([]byte(bucket)) == nil {
			return storage.ErrBucketNotFound
		}
		objects := tx.Bucket(bktObjects)
		k := objectKey(bucket, key)
		v := objects.Get(k)
		if v == nil {
			if ifMatch != "" {
				return storage.ErrObjectNotFound
			}
			return nil // borrar lo que no existe no es un error en S3
		}
		var rec objectRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		if err := checkPrecondition(&rec, storage.Precondition{IfMatch: ifMatch}); err != nil {
			return err
		}
		old = rec.Blob
		return objects.Delete(k)
	})
	if err == nil {
		b.removeBlobs(old)
	}
	return err
}

func (b *Backend) ListObjects(_ context.Context, bucket string, in storage.ListInput) (storage.ListOutput, error) {
	var out storage.ListOutput
	err := b.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bktBuckets).Get([]byte(bucket)) == nil {
			return storage.ErrBucketNotFound
		}
		if in.MaxKeys <= 0 {
			return nil
		}
		base := objectKey(bucket, "")
		prefix := append(bytes.Clone(base), in.Prefix...)

		start := prefix
		if in.After != "" {
			var after []byte
			if in.AfterPrefix {
				after = successor(append(bytes.Clone(base), in.After...))
				if after == nil {
					return nil
				}
			} else {
				// La menor clave estrictamente mayor que After.
				after = append(append(bytes.Clone(base), in.After...), 0)
			}
			if bytes.Compare(after, start) > 0 {
				start = after
			}
		}

		c := tx.Bucket(bktObjects).Cursor()
		count := 0
		for k, v := c.Seek(start); k != nil && bytes.HasPrefix(k, prefix); {
			if count == in.MaxKeys {
				out.Truncated = true
				break
			}
			key := string(k[len(base):])
			if in.Delimiter != "" {
				if i := strings.Index(key[len(in.Prefix):], in.Delimiter); i >= 0 {
					common := key[:len(in.Prefix)+i+len(in.Delimiter)]
					out.CommonPrefixes = append(out.CommonPrefixes, common)
					out.Next, out.NextIsPrefix = common, true
					count++
					next := successor(append(bytes.Clone(base), common...))
					if next == nil {
						break
					}
					k, v = c.Seek(next)
					continue
				}
			}
			var rec objectRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			rec.Key = key
			out.Objects = append(out.Objects, rec.Object)
			out.Next, out.NextIsPrefix = key, false
			count++
			k, v = c.Next()
		}
		return nil
	})
	return out, err
}

// ---- multipart ----

func (b *Backend) CreateUpload(_ context.Context, u storage.Upload) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	u.Initiated = time.Now().UTC()
	err = b.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bktBuckets).Get([]byte(u.Bucket)) == nil {
			return storage.ErrBucketNotFound
		}
		v, err := json.Marshal(u)
		if err != nil {
			return err
		}
		return tx.Bucket(bktUploads).Put([]byte(id), v)
	})
	return id, err
}

func getUpload(tx *bolt.Tx, bucket, key, id string) (storage.Upload, error) {
	var u storage.Upload
	v := tx.Bucket(bktUploads).Get([]byte(id))
	if v == nil {
		return u, storage.ErrUploadNotFound
	}
	if err := json.Unmarshal(v, &u); err != nil {
		return u, err
	}
	if u.Bucket != bucket || u.Key != key {
		return u, storage.ErrUploadNotFound
	}
	u.ID = id
	return u, nil
}

func (b *Backend) GetUpload(_ context.Context, bucket, key, id string) (storage.Upload, error) {
	var u storage.Upload
	err := b.db.View(func(tx *bolt.Tx) (err error) {
		u, err = getUpload(tx, bucket, key, id)
		return err
	})
	return u, err
}

func (b *Backend) PutPart(_ context.Context, bucket, key, id string, number int, s storage.Staged, in storage.PutInput) (storage.Part, error) {
	st, err := b.publish(s)
	if err != nil {
		s.Discard()
		return storage.Part{}, err
	}
	rec := partRecord{Blob: st.id, Part: storage.Part{
		Number: number, Size: st.size, ETag: in.ETag, Modified: time.Now().UTC(),
		ChecksumAlgo: in.ChecksumAlgo, Checksum: in.Checksum,
	}}
	var old string
	err = b.db.Update(func(tx *bolt.Tx) error {
		if _, err := getUpload(tx, bucket, key, id); err != nil {
			return err
		}
		parts := tx.Bucket(bktParts)
		k := partKey(id, number)
		if prev := parts.Get(k); prev != nil {
			var p partRecord
			if err := json.Unmarshal(prev, &p); err != nil {
				return err
			}
			old = p.Blob
		}
		v, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return parts.Put(k, v)
	})
	if err != nil {
		b.removeBlobs(st.id)
		return storage.Part{}, err
	}
	b.removeBlobs(old)
	return rec.Part, nil
}

func listParts(tx *bolt.Tx, id string) ([]partRecord, error) {
	var out []partRecord
	prefix := partKey(id, 0)[:len(id)+1]
	c := tx.Bucket(bktParts).Cursor()
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		var rec partRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(string(k[len(prefix):]))
		if err != nil {
			return nil, err
		}
		rec.Number = n
		out = append(out, rec)
	}
	return out, nil
}

func (b *Backend) ListParts(_ context.Context, bucket, key, id string) ([]storage.Part, error) {
	var out []storage.Part
	err := b.db.View(func(tx *bolt.Tx) error {
		if _, err := getUpload(tx, bucket, key, id); err != nil {
			return err
		}
		recs, err := listParts(tx, id)
		for _, r := range recs {
			out = append(out, r.Part)
		}
		return err
	})
	return out, err
}

func (b *Backend) ListUploads(_ context.Context, bucket string) ([]storage.Upload, error) {
	var out []storage.Upload
	err := b.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bktBuckets).Get([]byte(bucket)) == nil {
			return storage.ErrBucketNotFound
		}
		return tx.Bucket(bktUploads).ForEach(func(k, v []byte) error {
			var u storage.Upload
			if err := json.Unmarshal(v, &u); err != nil {
				return err
			}
			if u.Bucket == bucket {
				u.ID = string(k)
				out = append(out, u)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].ID < out[j].ID
	})
	return out, err
}

func (b *Backend) CompleteUpload(ctx context.Context, bucket, key, id string, completed []storage.CompletedPart, wantChecksum string, cond storage.Precondition) (storage.Object, error) {
	var upload storage.Upload
	var have []partRecord
	var done *objectRecord // el objeto, si esta misma subida ya se completó
	err := b.db.View(func(tx *bolt.Tx) (err error) {
		var current *objectRecord
		if v := tx.Bucket(bktObjects).Get(objectKey(bucket, key)); v != nil {
			current = new(objectRecord)
			if err := json.Unmarshal(v, current); err != nil {
				return err
			}
		}
		if upload, err = getUpload(tx, bucket, key, id); err != nil {
			if errors.Is(err, storage.ErrUploadNotFound) && current != nil && current.UploadID == id {
				done, err = current, nil
			}
			return err
		}
		// Se evalúa aquí para no unir las partes en vano; la comprobación que
		// vale es la de la transacción final.
		if err := checkPrecondition(current, cond); err != nil {
			return err
		}
		have, err = listParts(tx, id)
		return err
	})
	if err != nil {
		return storage.Object{}, err
	}
	if done != nil {
		done.Key = key
		return done.Object, nil
	}
	if len(completed) == 0 {
		return storage.Object{}, storage.ErrInvalidPart
	}
	byNumber := make(map[int]partRecord, len(have))
	for _, p := range have {
		byNumber[p.Number] = p
	}
	chosen := make([]partRecord, 0, len(completed))
	for i, c := range completed {
		if i > 0 && c.Number <= completed[i-1].Number {
			return storage.Object{}, storage.ErrPartOrder
		}
	}
	for i, c := range completed {
		p, ok := byNumber[c.Number]
		if !ok || p.ETag != c.ETag {
			return storage.Object{}, storage.ErrInvalidPart
		}
		if i < len(completed)-1 && p.Size < storage.MinPartSize {
			return storage.Object{}, storage.ErrPartTooSmall
		}
		chosen = append(chosen, p)
	}

	// El objeto final es un solo blob: se concatenan las partes.
	etags := md5.New()
	full := checksum.New(upload.ChecksumAlgo)
	if !upload.ChecksumFullObject {
		full = nil
	}
	var partSums []string
	pr, pw := io.Pipe()
	go func() {
		var w io.Writer = pw
		if full != nil {
			w = io.MultiWriter(pw, full)
		}
		for _, p := range chosen {
			if err := b.copyBlob(w, p.Blob); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()
	st, err := b.Stage(ctx, pr)
	pr.Close()
	if err != nil {
		return storage.Object{}, err
	}
	for _, p := range chosen {
		raw, err := hex.DecodeString(p.ETag)
		if err != nil {
			st.Discard()
			return storage.Object{}, storage.ErrInvalidPart
		}
		etags.Write(raw)
		partSums = append(partSums, p.Checksum)
	}

	rec := objectRecord{Object: storage.Object{
		Key: key, Size: st.Size(), Modified: time.Now().UTC(), Metadata: upload.Metadata, UploadID: id,
		ETag: hex.EncodeToString(etags.Sum(nil)) + "-" + strconv.Itoa(len(chosen)),
	}}
	if upload.ChecksumAlgo != "" {
		if full != nil {
			rec.ChecksumAlgo, rec.Checksum = upload.ChecksumAlgo, checksum.Encode(full)
		} else if sum, ok := checksum.Composite(upload.ChecksumAlgo, partSums); ok && !contains(partSums, "") {
			rec.ChecksumAlgo, rec.Checksum = upload.ChecksumAlgo, sum
		}
	}
	if wantChecksum != "" && wantChecksum != rec.Checksum {
		st.Discard()
		return storage.Object{}, storage.ErrBadDigest
	}

	pub, err := b.publish(st)
	if err != nil {
		st.Discard()
		return storage.Object{}, err
	}
	rec.Blob, rec.SHA256 = pub.id, pub.sha256
	if err := b.writeSidecar(bucket, key, rec); err != nil {
		b.removeBlobs(pub.id)
		return storage.Object{}, diskFull(err)
	}
	var garbage []string
	err = b.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bktBuckets).Get([]byte(bucket)) == nil {
			return storage.ErrBucketNotFound
		}
		// Otra petición pudo completar o abortar la subida mientras tanto.
		if _, err := getUpload(tx, bucket, key, id); err != nil {
			return err
		}
		old, err := putObject(tx, bucket, key, rec, cond)
		if err != nil {
			return err
		}
		removed, err := deleteUpload(tx, id)
		garbage = append(removed, old)
		return err
	})
	if err != nil {
		b.removeBlobs(pub.id)
		return storage.Object{}, err
	}
	b.removeBlobs(garbage...)
	return rec.Object, nil
}

func (b *Backend) copyBlob(w io.Writer, id string) error {
	f, err := os.Open(b.blobPath(id))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (b *Backend) AbortUpload(_ context.Context, bucket, key, id string) error {
	var blobs []string
	err := b.db.Update(func(tx *bolt.Tx) error {
		if _, err := getUpload(tx, bucket, key, id); err != nil {
			return err
		}
		var err error
		blobs, err = deleteUpload(tx, id)
		return err
	})
	if err == nil {
		b.removeBlobs(blobs...)
	}
	return err
}

// deleteUpload borra la subida y sus partes del índice y devuelve los blobs
// que quedaron sin referencia.
func deleteUpload(tx *bolt.Tx, id string) ([]string, error) {
	recs, err := listParts(tx, id)
	if err != nil {
		return nil, err
	}
	parts := tx.Bucket(bktParts)
	blobs := make([]string, 0, len(recs))
	for _, r := range recs {
		blobs = append(blobs, r.Blob)
		if err := parts.Delete(partKey(id, r.Number)); err != nil {
			return nil, err
		}
	}
	return blobs, tx.Bucket(bktUploads).Delete([]byte(id))
}

// ---- utilidades ----

// objectKey arma la clave del índice. El nombre de la chuspa nunca contiene
// el byte 0, así que el separador no es ambiguo sea cual sea la key.
func objectKey(bucket, key string) []byte {
	return []byte(bucket + "\x00" + key)
}

func partKey(uploadID string, number int) []byte {
	return []byte(fmt.Sprintf("%s\x00%05d", uploadID, number))
}

// successor devuelve la menor secuencia mayor que todas las que empiezan por
// p, o nil si no existe.
func successor(p []byte) []byte {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] != 0xff {
			out := bytes.Clone(p[:i+1])
			out[i]++
			return out
		}
	}
	return nil
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

package fsbolt

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	bberrors "go.etcd.io/bbolt/errors"
	"golang.org/x/sys/unix"

	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
)

// Formato de los datos en disco. Cada cambio incompatible sube este número y
// agrega su migración en migrate; los datos de una versión más nueva no se
// abren, para no dañarlos con código que no los entiende.
//
//	1  v0.1.0 y v0.2.0: índice y blobs.
//	2  v0.3.0: además, metadatos junto a cada blob (<blob>.meta), una marca
//	   por chuspa (chuspas/<nombre>) y SHA-256 de cada objeto en el índice.
//	   Con eso el índice se puede reconstruir y el contenido verificar.
const formatVersion = 2

var (
	bktMeta   = []byte("meta")
	keyFormat = []byte("format")
)

const (
	sidecarSuffix = ".meta"
	tmpSuffix     = ".tmp"
	markersDir    = "chuspas"
	recoveredDir  = "recuperados"
)

// sidecar es lo que se guarda junto a cada blob de objeto: basta para
// reconstruir su entrada en el índice.
type sidecar struct {
	Bucket string       `json:"bucket"`
	Key    string       `json:"key"`
	Record objectRecord `json:"record"`
}

// checkFormat lee la versión del formato y migra los datos antiguos.
func (b *Backend) checkFormat(readOnly bool) error {
	var current int
	empty := true
	err := b.db.View(func(tx *bolt.Tx) error {
		if m := tx.Bucket(bktMeta); m != nil {
			if v := m.Get(keyFormat); v != nil {
				n, err := strconv.Atoi(string(v))
				if err != nil {
					return fmt.Errorf("versión de formato ilegible %q", v)
				}
				current = n
			}
		}
		for _, name := range [][]byte{bktBuckets, bktObjects} {
			if bk := tx.Bucket(name); bk != nil {
				if k, _ := bk.Cursor().First(); k != nil {
					empty = false
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	switch {
	case current > formatVersion:
		return fmt.Errorf("los datos tienen el formato %d y esta versión de chuspa solo entiende hasta el %d: use una versión más nueva", current, formatVersion)
	case current == formatVersion || readOnly:
		return nil
	case current == 0 && empty:
		return b.setFormat()
	}
	return b.migrate()
}

func (b *Backend) setFormat() error {
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktMeta).Put(keyFormat, []byte(strconv.Itoa(formatVersion)))
	})
}

// migrate lleva los datos del formato 1 al 2: escribe la marca de cada
// chuspa y los metadatos junto a cada blob. Es idempotente: si se interrumpe,
// la siguiente apertura la repite.
func (b *Backend) migrate() error {
	var buckets []storage.Bucket
	type entry struct {
		bucket, key string
		rec         objectRecord
	}
	var objects []entry
	err := b.db.View(func(tx *bolt.Tx) error {
		err := tx.Bucket(bktBuckets).ForEach(func(_, v []byte) error {
			var bk storage.Bucket
			if err := json.Unmarshal(v, &bk); err != nil {
				return err
			}
			buckets = append(buckets, bk)
			return nil
		})
		if err != nil {
			return err
		}
		return tx.Bucket(bktObjects).ForEach(func(k, v []byte) error {
			bucket, key, ok := bytes.Cut(k, []byte{0})
			if !ok {
				return fmt.Errorf("clave de índice inválida %q", k)
			}
			var rec objectRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			objects = append(objects, entry{string(bucket), string(key), rec})
			return nil
		})
	})
	if err != nil {
		return err
	}
	for _, bk := range buckets {
		if err := b.writeMarker(bk); err != nil {
			return err
		}
	}
	for _, o := range objects {
		if err := b.writeSidecar(o.bucket, o.key, o.rec); err != nil {
			return err
		}
	}
	return b.setFormat()
}

// ---- metadatos junto a los blobs ----

func (b *Backend) sidecarPath(id string) string { return b.blobPath(id) + sidecarSuffix }

func (b *Backend) markerPath(bucket string) string {
	return filepath.Join(b.dir, markersDir, bucket)
}

func (b *Backend) writeSidecar(bucket, key string, rec objectRecord) error {
	data, err := json.Marshal(sidecar{Bucket: bucket, Key: key, Record: rec})
	if err != nil {
		return err
	}
	return writeFileSync(b.sidecarPath(rec.Blob), data)
}

func (b *Backend) writeMarker(bk storage.Bucket) error {
	data, err := json.Marshal(bk)
	if err != nil {
		return err
	}
	return writeFileSync(b.markerPath(bk.Name), data)
}

func (b *Backend) removeMarker(bucket string) {
	if os.Remove(b.markerPath(bucket)) == nil {
		_ = syncDir(filepath.Join(b.dir, markersDir))
	}
}

// writeFileSync escribe path de forma atómica y durable.
func writeFileSync(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// blobID devuelve el id del blob al que pertenece un archivo de blobs/.
func blobID(name string) string {
	name = strings.TrimSuffix(name, tmpSuffix)
	return strings.TrimSuffix(name, sidecarSuffix)
}

// ---- espacio en disco ----

// checkSpace rechaza escrituras cuando el disco tiene menos de MinFreeBytes
// libres: un índice o un blob que no caben son peores que un error claro.
func (b *Backend) checkSpace() error {
	if b.MinFreeBytes <= 0 {
		return nil
	}
	free, err := FreeBytes(b.dir)
	if err != nil {
		return err
	}
	if free < uint64(b.MinFreeBytes) {
		return storage.ErrInsufficientStorage
	}
	return nil
}

// FreeBytes devuelve el espacio disponible para el usuario en el disco de dir.
func FreeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert // los tipos cambian entre Linux y macOS
}

func diskFull(err error) error {
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return storage.ErrInsufficientStorage
	}
	return err
}

// ---- verificación ----

// Problem es un objeto cuyo contenido no coincide con lo que dice el índice.
type Problem struct {
	Bucket, Key, Blob, Reason string
}

// VerifyResult resume una verificación.
type VerifyResult struct {
	Objects int
	Bytes   int64
	Skipped int // objetos reemplazados o borrados durante la verificación
}

// Verify relee el contenido de todos los objetos y lo compara con el tamaño y
// el hash guardados. Se puede correr con el servidor atendiendo: el índice se
// lee por tandas y un objeto que cambia mientras tanto se salta.
func (b *Backend) Verify(ctx context.Context, report func(Problem)) (VerifyResult, error) {
	var res VerifyResult
	var after []byte
	for {
		type item struct {
			k   []byte
			rec objectRecord
		}
		var batch []item
		err := b.db.View(func(tx *bolt.Tx) error {
			c := tx.Bucket(bktObjects).Cursor()
			k, v := c.First()
			if after != nil {
				k, v = c.Seek(after)
				if k != nil && bytes.Equal(k, after) {
					k, v = c.Next()
				}
			}
			for ; k != nil && len(batch) < 500; k, v = c.Next() {
				var rec objectRecord
				if err := json.Unmarshal(v, &rec); err != nil {
					return err
				}
				batch = append(batch, item{bytes.Clone(k), rec})
			}
			return nil
		})
		if err != nil || len(batch) == 0 {
			return res, err
		}
		for _, it := range batch {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			bucket, key, _ := bytes.Cut(it.k, []byte{0})
			reason, err := b.verifyObject(it.rec)
			if err != nil {
				return res, err
			}
			if reason != "" {
				// Puede que el objeto haya cambiado mientras se leía.
				if cur, err := b.getRecord(string(bucket), string(key)); err != nil || cur.Blob != it.rec.Blob {
					res.Skipped++
					continue
				}
				report(Problem{Bucket: string(bucket), Key: string(key), Blob: it.rec.Blob, Reason: reason})
			}
			res.Objects++
			res.Bytes += it.rec.Size
		}
		after = batch[len(batch)-1].k
	}
}

// verifyObject devuelve por qué el blob no coincide con su registro, o "".
func (b *Backend) verifyObject(rec objectRecord) (string, error) {
	f, err := os.Open(b.blobPath(rec.Blob))
	if errors.Is(err, fs.ErrNotExist) {
		return "falta el blob", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()

	var h hash.Hash
	var want string
	switch {
	case rec.SHA256 != "":
		h, want = sha256.New(), rec.SHA256
	case !strings.Contains(rec.ETag, "-"):
		// Objetos del formato 1: el ETag de un PUT simple es el MD5.
		h, want = md5.New(), rec.ETag
	}
	var n int64
	if h != nil {
		n, err = io.Copy(h, f)
	} else {
		n, err = io.Copy(io.Discard, f)
	}
	if err != nil {
		return "", err
	}
	switch {
	case n != rec.Size:
		return fmt.Sprintf("tamaño %d, se esperaban %d bytes", n, rec.Size), nil
	case h != nil && hex.EncodeToString(h.Sum(nil)) != want:
		return "el contenido no coincide con su hash", nil
	}
	if _, err := os.Stat(b.sidecarPath(rec.Blob)); errors.Is(err, fs.ErrNotExist) {
		return "faltan los metadatos junto al blob", nil
	}
	return "", nil
}

// ---- reconstrucción del índice ----

// RebuildResult resume una reconstrucción del índice.
type RebuildResult struct {
	Buckets   int
	Objects   int
	Recovered int    // archivos apartados en recuperados/
	OldIndex  string // a dónde se movió el índice anterior, si había
}

// Rebuild crea un índice nuevo a partir de las marcas de chuspa y de los
// metadatos guardados junto a cada blob. Necesita el servidor detenido.
//
// No borra nada: el índice anterior se renombra, y los blobs que no se pueden
// asignar a un objeto (partes de subidas sin completar, versiones viejas de
// un objeto, blobs sin metadatos) se mueven a recuperados/.
func Rebuild(dir string) (RebuildResult, error) {
	var res RebuildResult
	index := filepath.Join(dir, "meta.db")
	if _, err := os.Stat(index); err == nil {
		// Si un servidor lo tiene abierto, bbolt no deja tomarlo. Un índice
		// dañado tampoco abre, y es justo el caso de reconstruirlo.
		db, err := bolt.Open(index, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
		if errors.Is(err, bberrors.ErrTimeout) {
			return res, errors.New("el índice está en uso: detenga el servidor antes de reconstruirlo")
		}
		if err == nil {
			db.Close()
		}
		res.OldIndex = index + ".reemplazado-" + time.Now().UTC().Format("20060102T150405Z")
		if err := os.Rename(index, res.OldIndex); err != nil {
			return res, err
		}
	}

	buckets := map[string]storage.Bucket{}
	entries, err := os.ReadDir(filepath.Join(dir, markersDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), tmpSuffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, markersDir, e.Name()))
		if err != nil {
			return res, err
		}
		var bk storage.Bucket
		if err := json.Unmarshal(data, &bk); err != nil || bk.Name != e.Name() {
			continue
		}
		buckets[bk.Name] = bk
	}

	type found struct {
		bucket, key string
		rec         objectRecord
	}
	winners := map[string]found{}
	var losers []string // ids de blobs a apartar
	blobs := map[string]bool{}
	err = filepath.WalkDir(filepath.Join(dir, "blobs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		switch {
		case strings.HasSuffix(name, tmpSuffix):
			return os.Remove(path)
		case !strings.HasSuffix(name, sidecarSuffix):
			blobs[name] = true
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var sc sidecar
		id := blobID(name)
		if json.Unmarshal(data, &sc) != nil || sc.Record.Blob != id || sc.Bucket == "" {
			losers = append(losers, id)
			return nil
		}
		info, err := os.Stat(strings.TrimSuffix(path, sidecarSuffix))
		if err != nil || info.Size() != sc.Record.Size {
			losers = append(losers, id)
			return nil
		}
		k := string(objectKey(sc.Bucket, sc.Key))
		if prev, ok := winners[k]; ok {
			if !sc.Record.Modified.After(prev.rec.Modified) {
				losers = append(losers, id)
				return nil
			}
			losers = append(losers, prev.rec.Blob)
		}
		winners[k] = found{sc.Bucket, sc.Key, sc.Record}
		return nil
	})
	if err != nil {
		return res, err
	}

	b, _, err := openDir(dir, false)
	if err != nil {
		return res, err
	}
	defer b.Close()
	for _, w := range winners {
		if _, ok := buckets[w.bucket]; !ok {
			buckets[w.bucket] = storage.Bucket{Name: w.bucket, Created: w.rec.Modified}
			if err := b.writeMarker(buckets[w.bucket]); err != nil {
				return res, err
			}
		}
	}
	err = b.db.Update(func(tx *bolt.Tx) error {
		for _, bk := range buckets {
			v, _ := json.Marshal(bk)
			if err := tx.Bucket(bktBuckets).Put([]byte(bk.Name), v); err != nil {
				return err
			}
		}
		for k, w := range winners {
			v, err := json.Marshal(w.rec)
			if err != nil {
				return err
			}
			if err := tx.Bucket(bktObjects).Put([]byte(k), v); err != nil {
				return err
			}
			delete(blobs, w.rec.Blob)
		}
		return tx.Bucket(bktMeta).Put(keyFormat, []byte(strconv.Itoa(formatVersion)))
	})
	if err != nil {
		return res, err
	}
	res.Buckets, res.Objects = len(buckets), len(winners)

	// Lo que no quedó en el índice se aparta en lugar de borrarse.
	for _, id := range losers {
		blobs[id] = true
	}
	for id := range blobs {
		for _, p := range []string{b.blobPath(id), b.sidecarPath(id)} {
			dst := filepath.Join(dir, recoveredDir, filepath.Base(p))
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return res, err
			}
			if err := os.Rename(p, dst); err == nil && p == b.blobPath(id) {
				res.Recovered++
			}
		}
	}
	return res, nil
}

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var commands = map[string]func(context.Context, *s3.Client, []string) error{
	"mira":       mira,
	"crea":       crea,
	"subi":       subi,
	"baja":       baja,
	"borra":      borra,
	"sincroniza": sincroniza,
}

// out es la salida de los comandos; las pruebas la reemplazan.
var out io.Writer = os.Stdout

const (
	// Por encima de este tamaño la subida va en partes.
	multipartThreshold = 64 << 20
	minPartSize        = 16 << 20
	maxParts           = 10000
)

func mira(ctx context.Context, c *s3.Client, args []string) error {
	pos, flags, err := splitFlags(args, "-r", "--todo")
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef("mira recibe una sola dirección")
	}
	loc := location{}
	if len(pos) == 1 {
		if loc, err = parseLocation(pos[0]); err != nil {
			return err
		}
	}
	if loc.Bucket == "" {
		res, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err != nil {
			return err
		}
		for _, b := range res.Buckets {
			fmt.Fprintf(out, "%s  chuspa://%s\n", stamp(b.CreationDate), aws.ToString(b.Name))
		}
		return nil
	}

	in := &s3.ListObjectsV2Input{Bucket: aws.String(loc.Bucket), Prefix: aws.String(loc.Key)}
	if !flags["-r"] && !flags["--todo"] {
		in.Delimiter = aws.String("/")
	}
	p := s3.NewListObjectsV2Paginator(c, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return target(loc, err)
		}
		for _, cp := range page.CommonPrefixes {
			fmt.Fprintf(out, "%16s  %12s  %s\n", "", "CARPETA", aws.ToString(cp.Prefix))
		}
		for _, o := range page.Contents {
			fmt.Fprintf(out, "%s  %12d  %s\n", stamp(o.LastModified), aws.ToInt64(o.Size), aws.ToString(o.Key))
		}
	}
	return nil
}

func crea(ctx context.Context, c *s3.Client, args []string) error {
	if len(args) != 1 {
		return usagef("crea recibe una dirección chuspa://nombre")
	}
	loc, err := parseLocation(args[0])
	if err != nil {
		return err
	}
	if loc.Bucket == "" || strings.Trim(loc.Key, "/") != "" {
		return usagef("crea recibe solo el nombre de la chuspa, sin key")
	}
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(loc.Bucket)}); err != nil {
		return target(loc, err)
	}
	fmt.Fprintf(out, "chuspa creada: chuspa://%s\n", loc.Bucket)
	return nil
}

func subi(ctx context.Context, c *s3.Client, args []string) error {
	args = dropConnector(args)
	if len(args) < 2 || !isRemote(args[len(args)-1]) {
		return usagef("subi recibe uno o más archivos y un destino chuspa://chuspa/key")
	}
	dst, err := parseLocation(args[len(args)-1])
	if err != nil {
		return err
	}
	if dst.Bucket == "" {
		return usagef("falta la chuspa de destino")
	}
	files := args[:len(args)-1]
	if len(files) > 1 && dst.Key != "" && !strings.HasSuffix(dst.Key, "/") {
		return usagef("con varios archivos el destino debe terminar en \"/\"")
	}
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return usagef("%s es una carpeta: para carpetas use ve sincroniza", f)
		}
		to := location{Bucket: dst.Bucket, Key: uploadKey(dst, f)}
		if err := upload(ctx, c, f, info.Size(), to); err != nil {
			return target(to, err)
		}
		fmt.Fprintf(out, "subido: %s → %s\n", f, to)
	}
	return nil
}

func baja(ctx context.Context, c *s3.Client, args []string) error {
	args = dropConnector(args)
	if len(args) == 1 {
		args = append(args, ".")
	}
	if len(args) != 2 || !isRemote(args[0]) {
		return usagef("baja recibe chuspa://chuspa/key y, opcional, un destino")
	}
	src, err := parseLocation(args[0])
	if err != nil {
		return err
	}
	if src.Bucket == "" || src.Key == "" || strings.HasSuffix(src.Key, "/") {
		return usagef("baja descarga un objeto: indique chuspa://chuspa/key (para carpetas use ve sincroniza)")
	}
	dest := args[1]
	if info, err := os.Stat(dest); (err == nil && info.IsDir()) || strings.HasSuffix(dest, "/") || strings.HasSuffix(dest, string(os.PathSeparator)) {
		name := path.Base(src.Key)
		if name == "." || name == ".." || name == "/" {
			return fmt.Errorf("la key %q no sirve como nombre de archivo: indique el destino completo", src.Key)
		}
		dest = filepath.Join(dest, name)
	}
	n, err := download(ctx, c, src, dest)
	if err != nil {
		return target(src, err)
	}
	fmt.Fprintf(out, "bajado: %s → %s (%d bytes)\n", src, dest, n)
	return nil
}

func borra(ctx context.Context, c *s3.Client, args []string) error {
	pos, flags, err := splitFlags(args, "-r", "--todo")
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("borra recibe una dirección chuspa://chuspa[/key]")
	}
	loc, err := parseLocation(pos[0])
	if err != nil {
		return err
	}
	if loc.Bucket == "" {
		return usagef("falta la chuspa")
	}
	recursive := flags["-r"] || flags["--todo"]

	if loc.Key != "" && !recursive {
		if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key)}); err != nil {
			if strings.HasSuffix(loc.Key, "/") && apiCode(err) == "NotFound" {
				return usagef("%s parece una carpeta: use -r para borrar todo lo que tiene", loc)
			}
			return target(loc, err)
		}
		if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(loc.Bucket), Key: aws.String(loc.Key)}); err != nil {
			return target(loc, err)
		}
		fmt.Fprintf(out, "borrado: %s\n", loc)
		return nil
	}

	if recursive {
		n, err := deletePrefix(ctx, c, loc)
		if err != nil {
			return target(loc, err)
		}
		if loc.Key != "" {
			if n == 0 {
				return target(loc, errors.New("no hay nada bajo ese prefijo"))
			}
			fmt.Fprintf(out, "borrados %d objetos bajo %s\n", n, loc)
			return nil
		}
		fmt.Fprintf(out, "borrados %d objetos de chuspa://%s\n", n, loc.Bucket)
	}
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(loc.Bucket)}); err != nil {
		return target(loc, err)
	}
	fmt.Fprintf(out, "chuspa borrada: chuspa://%s\n", loc.Bucket)
	return nil
}

func sincroniza(ctx context.Context, c *s3.Client, args []string) error {
	if len(args) != 2 || isRemote(args[0]) == isRemote(args[1]) {
		return usagef("sincroniza recibe una carpeta local y una dirección chuspa://, en cualquier orden")
	}
	if isRemote(args[1]) {
		return syncUp(ctx, c, args[0], args[1])
	}
	return syncDown(ctx, c, args[0], args[1])
}

func syncUp(ctx context.Context, c *s3.Client, dir, dstArg string) error {
	dst, err := parseLocation(dstArg)
	if err != nil {
		return err
	}
	if dst.Bucket == "" {
		return usagef("falta la chuspa de destino")
	}
	if info, err := os.Stat(dir); err != nil {
		return err
	} else if !info.IsDir() {
		return usagef("%s no es una carpeta: para un archivo use ve subi", dir)
	}
	prefix := dirPrefix(dst.Key)
	remote, err := listRemote(ctx, c, dst.Bucket, prefix)
	if err != nil {
		return target(dst, err)
	}

	copied, skipped := 0, 0
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			fmt.Fprintf(os.Stderr, "ve: me salto %s: no es un archivo regular\n", p)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		to := location{Bucket: dst.Bucket, Key: prefix + filepath.ToSlash(rel)}
		if r, ok := remote[to.Key]; ok && !needsUpload(info, r) {
			skipped++
			return nil
		}
		if err := upload(ctx, c, p, info.Size(), to); err != nil {
			return target(to, err)
		}
		fmt.Fprintf(out, "subido: %s → %s\n", p, to)
		copied++
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sincronizado: %d subidos, %d sin cambios\n", copied, skipped)
	return nil
}

func syncDown(ctx context.Context, c *s3.Client, srcArg, dir string) error {
	src, err := parseLocation(srcArg)
	if err != nil {
		return err
	}
	if src.Bucket == "" {
		return usagef("falta la chuspa de origen")
	}
	prefix := dirPrefix(src.Key)
	remote, err := listRemote(ctx, c, src.Bucket, prefix)
	if err != nil {
		return target(src, err)
	}
	copied, skipped := 0, 0
	for key, r := range remote {
		rel := strings.TrimPrefix(key, prefix)
		local, ok := safeLocalPath(dir, rel)
		if !ok {
			fmt.Fprintf(os.Stderr, "ve: me salto la key %q: no se puede guardar como archivo dentro de %s\n", key, dir)
			continue
		}
		if info, err := os.Stat(local); err == nil && !needsDownload(info, r) {
			skipped++
			continue
		}
		from := location{Bucket: src.Bucket, Key: key}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		if _, err := download(ctx, c, from, local); err != nil {
			return target(from, err)
		}
		fmt.Fprintf(out, "bajado: %s → %s\n", from, local)
		copied++
	}
	fmt.Fprintf(out, "sincronizado: %d bajados, %d sin cambios\n", copied, skipped)
	return nil
}

// remoteObject es lo que la sincronización necesita de un objeto remoto.
type remoteObject struct {
	Size         int64
	LastModified time.Time
}

func listRemote(ctx context.Context, c *s3.Client, bucket, prefix string) (map[string]remoteObject, error) {
	objs := map[string]remoteObject{}
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			objs[aws.ToString(o.Key)] = remoteObject{Size: aws.ToInt64(o.Size), LastModified: aws.ToTime(o.LastModified)}
		}
	}
	return objs, nil
}

// La comparación es como la de "aws s3 sync": tamaño distinto o el origen es
// más nuevo. Las fechas se comparan al segundo porque es la precisión de
// Last-Modified.
func needsUpload(local fs.FileInfo, r remoteObject) bool {
	return local.Size() != r.Size || local.ModTime().Truncate(time.Second).After(r.LastModified.Truncate(time.Second))
}

func needsDownload(local fs.FileInfo, r remoteObject) bool {
	return local.Size() != r.Size || r.LastModified.Truncate(time.Second).After(local.ModTime().Truncate(time.Second))
}

// safeLocalPath convierte una key relativa en una ruta dentro de dir. Las keys
// son datos arbitrarios ("../../etc/passwd" es una key válida), así que se
// rechaza todo lo que se saldría de dir o no es un nombre de archivo.
func safeLocalPath(dir, rel string) (string, bool) {
	if rel == "" || strings.HasSuffix(rel, "/") || strings.ContainsRune(rel, 0) {
		return "", false
	}
	native := filepath.FromSlash(rel)
	if !filepath.IsLocal(native) {
		return "", false
	}
	return filepath.Join(dir, native), true
}

func dirPrefix(key string) string {
	if key == "" || strings.HasSuffix(key, "/") {
		return key
	}
	return key + "/"
}

func upload(ctx context.Context, c *s3.Client, file string, size int64, to location) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	contentType := mime.TypeByExtension(filepath.Ext(file))
	if size <= multipartThreshold {
		in := &s3.PutObjectInput{Bucket: aws.String(to.Bucket), Key: aws.String(to.Key), Body: f, ContentLength: aws.Int64(size)}
		if contentType != "" {
			in.ContentType = aws.String(contentType)
		}
		_, err = c.PutObject(ctx, in)
		return err
	}
	return uploadMultipart(ctx, c, f, size, to, contentType)
}

func uploadMultipart(ctx context.Context, c *s3.Client, f *os.File, size int64, to location, contentType string) error {
	in := &s3.CreateMultipartUploadInput{Bucket: aws.String(to.Bucket), Key: aws.String(to.Key)}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	mp, err := c.CreateMultipartUpload(ctx, in)
	if err != nil {
		return err
	}
	abort := func(cause error) error {
		// Con un contexto nuevo: si el usuario canceló, igual hay que avisarle al motor.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = c.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{Bucket: mp.Bucket, Key: mp.Key, UploadId: mp.UploadId})
		return cause
	}

	partSize := partSizeFor(size)
	var parts []types.CompletedPart
	for n, off := int32(1), int64(0); off < size; n, off = n+1, off+partSize {
		length := min(partSize, size-off)
		res, err := c.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: mp.Bucket, Key: mp.Key, UploadId: mp.UploadId, PartNumber: aws.Int32(n),
			Body: io.NewSectionReader(f, off, length), ContentLength: aws.Int64(length),
		})
		if err != nil {
			return abort(err)
		}
		parts = append(parts, types.CompletedPart{ETag: res.ETag, PartNumber: aws.Int32(n),
			ChecksumCRC32: res.ChecksumCRC32, ChecksumCRC32C: res.ChecksumCRC32C, ChecksumCRC64NVME: res.ChecksumCRC64NVME,
			ChecksumSHA1: res.ChecksumSHA1, ChecksumSHA256: res.ChecksumSHA256})
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: mp.Bucket, Key: mp.Key, UploadId: mp.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(err)
	}
	return nil
}

// partSizeFor elige el tamaño de parte: 16 MiB, o más si el archivo
// necesitaría más de 10 000 partes.
func partSizeFor(size int64) int64 {
	ps := int64(minPartSize)
	if need := (size + maxParts - 1) / maxParts; need > ps {
		ps = (need + 1<<20 - 1) &^ (1<<20 - 1)
	}
	return ps
}

// download escribe el objeto en dest de forma atómica: primero a un temporal
// en la misma carpeta y luego lo renombra. Deja la fecha del archivo igual a
// Last-Modified para que "sincroniza" no lo vuelva a bajar.
func download(ctx context.Context, c *s3.Client, src location, dest string) (int64, error) {
	res, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(src.Bucket), Key: aws.String(src.Key)})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".ve-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, res.Body)
	if err == nil && res.ContentLength != nil && n != *res.ContentLength {
		err = fmt.Errorf("descarga incompleta: %d de %d bytes", n, *res.ContentLength)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return 0, err
	}
	if res.LastModified != nil {
		_ = os.Chtimes(tmp.Name(), *res.LastModified, *res.LastModified)
	}
	return n, os.Rename(tmp.Name(), dest)
}

func deletePrefix(ctx context.Context, c *s3.Client, loc location) (int, error) {
	total := 0
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(loc.Bucket), Prefix: aws.String(loc.Key)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return total, err
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, o := range page.Contents {
			ids = append(ids, types.ObjectIdentifier{Key: o.Key})
		}
		res, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(loc.Bucket),
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return total, err
		}
		if len(res.Errors) > 0 {
			e := res.Errors[0]
			return total, fmt.Errorf("no se pudo borrar %q: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
		total += len(ids)
	}
	return total, nil
}

func stamp(t *time.Time) string {
	if t == nil {
		return fmt.Sprintf("%16s", "")
	}
	return t.Local().Format("2006-01-02 15:04")
}

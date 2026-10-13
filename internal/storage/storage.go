// Package storage define el contrato entre la API de S3 y el almacenamiento.
// Los handlers solo conocen la interfaz Backend, nunca el disco.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrBucketNotFound = errors.New("storage: la chuspa no existe")
	ErrBucketExists   = errors.New("storage: la chuspa ya existe")
	ErrBucketNotEmpty = errors.New("storage: la chuspa no está vacía")
	ErrObjectNotFound = errors.New("storage: el objeto no existe")
	ErrUploadNotFound = errors.New("storage: la subida multipart no existe")
	ErrInvalidPart    = errors.New("storage: parte inexistente o con ETag distinto")
	ErrPartOrder      = errors.New("storage: las partes no vienen en orden ascendente")
	ErrPartTooSmall   = errors.New("storage: una parte que no es la última es menor al mínimo")
	ErrBadDigest      = errors.New("storage: el checksum declarado no coincide con el contenido")
)

// MinPartSize es el tamaño mínimo de toda parte que no sea la última.
const MinPartSize = 5 << 20

// Bucket es una chuspa.
type Bucket struct {
	Name    string
	Created time.Time
}

// Metadata es lo que el cliente define al subir un objeto.
type Metadata struct {
	ContentType        string            `json:"contentType,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentLanguage    string            `json:"contentLanguage,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	User               map[string]string `json:"user,omitempty"`
}

// Object describe un objeto guardado.
type Object struct {
	Key      string    `json:"-"`
	Size     int64     `json:"size"`
	ETag     string    `json:"etag"` // sin comillas
	Modified time.Time `json:"modified"`
	Metadata
	// ChecksumAlgo y Checksum guardan el checksum declarado por el cliente y
	// verificado al subir; Checksum va en base64.
	ChecksumAlgo string `json:"checksumAlgo,omitempty"`
	Checksum     string `json:"checksum,omitempty"`
}

// Staged es contenido ya escrito y sincronizado en disco, pendiente de
// confirmar en el índice. Quien lo recibe debe confirmarlo o descartarlo.
type Staged interface {
	Size() int64
	// Discard borra el contenido. Es seguro llamarlo después de confirmar.
	Discard()
}

// PutInput acompaña al contenido al confirmar un objeto o una parte.
type PutInput struct {
	ETag         string
	ChecksumAlgo string
	Checksum     string
	Metadata     Metadata
}

// ListInput son los parámetros de un listado de objetos.
type ListInput struct {
	Prefix    string
	Delimiter string
	// After es exclusivo: el listado empieza después de esta key. Si
	// AfterPrefix es true, After es un prefijo común y se salta completo.
	After       string
	AfterPrefix bool
	MaxKeys     int
}

// ListOutput es una página de un listado, en orden lexicográfico.
type ListOutput struct {
	Objects        []Object
	CommonPrefixes []string
	Truncated      bool
	// Next y NextIsPrefix identifican el último elemento devuelto, para
	// continuar con ListInput.After y AfterPrefix.
	Next         string
	NextIsPrefix bool
}

// Upload es una subida multipart en curso.
type Upload struct {
	ID        string    `json:"-"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	Initiated time.Time `json:"initiated"`
	Metadata
	ChecksumAlgo string `json:"checksumAlgo,omitempty"`
	// ChecksumFullObject indica que el checksum final es del objeto completo
	// y no el compuesto de las partes.
	ChecksumFullObject bool `json:"checksumFullObject,omitempty"`
}

// Part es una parte subida.
type Part struct {
	Number       int       `json:"-"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	Modified     time.Time `json:"modified"`
	ChecksumAlgo string    `json:"checksumAlgo,omitempty"`
	Checksum     string    `json:"checksum,omitempty"`
}

// CompletedPart es una parte tal como la declara el cliente al completar.
type CompletedPart struct {
	Number int
	ETag   string
}

// Backend es el almacenamiento del motor.
type Backend interface {
	CreateBucket(ctx context.Context, name string) error
	GetBucket(ctx context.Context, name string) (Bucket, error)
	ListBuckets(ctx context.Context) ([]Bucket, error)
	DeleteBucket(ctx context.Context, name string) error

	// Stage escribe r en disco de forma durable sin hacerlo visible.
	Stage(ctx context.Context, r io.Reader) (Staged, error)

	PutObject(ctx context.Context, bucket, key string, s Staged, in PutInput) (Object, error)
	HeadObject(ctx context.Context, bucket, key string) (Object, error)
	// GetObject devuelve el objeto y su contenido; quien llama cierra el lector.
	GetObject(ctx context.Context, bucket, key string) (Object, io.ReadSeekCloser, error)
	DeleteObject(ctx context.Context, bucket, key string) error
	ListObjects(ctx context.Context, bucket string, in ListInput) (ListOutput, error)

	CreateUpload(ctx context.Context, u Upload) (string, error)
	GetUpload(ctx context.Context, bucket, key, uploadID string) (Upload, error)
	PutPart(ctx context.Context, bucket, key, uploadID string, number int, s Staged, in PutInput) (Part, error)
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]Part, error)
	// CompleteUpload une las partes declaradas y publica el objeto. Si
	// checksum no es vacío, debe coincidir con el del objeto resultante.
	CompleteUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart, checksum string) (Object, error)
	AbortUpload(ctx context.Context, bucket, key, uploadID string) error

	Close() error
}

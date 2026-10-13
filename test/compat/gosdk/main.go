// Flujo básico con aws-sdk-go-v2 como cliente, en su configuración por
// defecto salvo el estilo de ruta.
//
// Uso: go run . <endpoint> <bucket>. Credenciales y región por variables AWS_*.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("uso: gosdk <endpoint> <bucket>")
	}
	if err := run(context.Background(), os.Args[1], os.Args[2]); err != nil {
		log.Fatal("aws-sdk-go-v2: ", err)
	}
	fmt.Println("aws-sdk-go-v2: bien")
}

func run(ctx context.Context, endpoint, bucket string) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	// Objeto pequeño con metadatos
	small := []byte("oíste mira")
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: aws.String("gosdk/pequeño.txt"), Body: bytes.NewReader(small),
		ContentType: aws.String("text/plain"), Metadata: map[string]string{"barrio": "San Antonio"},
	})
	if err != nil {
		return fmt.Errorf("PutObject: %w", err)
	}
	got, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: aws.String("gosdk/pequeño.txt")})
	if err != nil {
		return fmt.Errorf("GetObject: %w", err)
	}
	data, err := io.ReadAll(got.Body) // aquí el SDK valida el checksum de la respuesta
	got.Body.Close()
	if err != nil || !bytes.Equal(data, small) || got.Metadata["barrio"] != "San Antonio" {
		return fmt.Errorf("GetObject devolvió %q, metadatos %v, error %v", data, got.Metadata, err)
	}

	// Rango
	part, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bucket, Key: aws.String("gosdk/pequeño.txt"), Range: aws.String("bytes=0-5")})
	if err != nil {
		return fmt.Errorf("GetObject con rango: %w", err)
	}
	data, _ = io.ReadAll(part.Body)
	part.Body.Close()
	if !bytes.Equal(data, small[:6]) {
		return fmt.Errorf("rango incorrecto: %q", data)
	}

	// Multipart con el gestor de transferencias
	big := make([]byte, 20<<20)
	rand.Read(big)
	if _, err := manager.NewUploader(client).Upload(ctx, &s3.PutObjectInput{
		Bucket: &bucket, Key: aws.String("gosdk/grande.bin"), Body: bytes.NewReader(big),
	}); err != nil {
		return fmt.Errorf("subida multipart: %w", err)
	}
	buf := manager.NewWriteAtBuffer(nil)
	if _, err := manager.NewDownloader(client).Download(ctx, buf, &s3.GetObjectInput{Bucket: &bucket, Key: aws.String("gosdk/grande.bin")}); err != nil {
		return fmt.Errorf("descarga por rangos: %w", err)
	}
	if !bytes.Equal(buf.Bytes(), big) {
		return errors.New("el archivo grande no volvió idéntico")
	}

	// Copia
	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: &bucket, Key: aws.String("gosdk/copia.txt"), CopySource: aws.String(bucket + "/gosdk/peque%C3%B1o.txt"),
	}); err != nil {
		return fmt.Errorf("CopyObject: %w", err)
	}

	// Listado paginado
	var keys []string
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: aws.String("gosdk/"), MaxKeys: aws.Int32(1)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("ListObjectsV2: %w", err)
		}
		for _, o := range page.Contents {
			keys = append(keys, *o.Key)
		}
	}
	if fmt.Sprint(keys) != "[gosdk/copia.txt gosdk/grande.bin gosdk/pequeño.txt]" {
		return fmt.Errorf("listado inesperado: %v", keys)
	}

	// Error tipado
	_, err = client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: aws.String("gosdk/no-existe")})
	var notFound *types.NotFound
	if !errors.As(err, &notFound) {
		return fmt.Errorf("HeadObject de un objeto inexistente: %v", err)
	}

	// Borrado masivo
	ids := make([]types.ObjectIdentifier, len(keys))
	for i := range keys {
		ids[i] = types.ObjectIdentifier{Key: &keys[i]}
	}
	out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &types.Delete{Objects: ids}})
	if err != nil || len(out.Deleted) != len(keys) || len(out.Errors) != 0 {
		return fmt.Errorf("DeleteObjects: %v, borrados %d, errores %v", err, len(out.Deleted), out.Errors)
	}
	return nil
}

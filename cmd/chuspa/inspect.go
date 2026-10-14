package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/OpenChuspa/OpenChuspa-core/internal/config"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage"
	"github.com/OpenChuspa/OpenChuspa-core/internal/storage/fsbolt"
)

// Los datos no se pueden mirar con ls porque las keys viven en el índice;
// inspect y export son la forma de verlos y sacarlos sin pasar por la API.

func openForInspection() (*fsbolt.Backend, error) {
	dir := os.Getenv("CHUSPA_DATA_DIR")
	if dir == "" {
		dir = config.DefaultDataDir
	}
	return fsbolt.OpenReadOnly(dir)
}

// eachObject recorre todos los objetos de la chuspa que empiezan por prefix.
func eachObject(ctx context.Context, b *fsbolt.Backend, bucket, prefix string, fn func(storage.Object)) error {
	in := storage.ListInput{Prefix: prefix, MaxKeys: 1000}
	for {
		page, err := b.ListObjects(ctx, bucket, in)
		if err != nil {
			return err
		}
		for _, o := range page.Objects {
			fn(o)
		}
		if !page.Truncated {
			return nil
		}
		in.After = page.Next
	}
}

func inspect(args []string) error {
	if len(args) > 2 {
		return errors.New("uso: chuspa inspect [chuspa [prefijo]]")
	}
	b, err := openForInspection()
	if err != nil {
		return err
	}
	defer b.Close()
	ctx := context.Background()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()

	if len(args) == 0 {
		buckets, err := b.ListBuckets(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "CHUSPA\tCREADA\tOBJETOS\tBYTES")
		for _, bk := range buckets {
			var count, size int64
			if err := eachObject(ctx, b, bk.Name, "", func(o storage.Object) { count++; size += o.Size }); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", bk.Name, bk.Created.Format(time.RFC3339), count, size)
		}
		return nil
	}

	prefix := ""
	if len(args) == 2 {
		prefix = args[1]
	}
	fmt.Fprintln(w, "BYTES\tMODIFICADO\tETAG\tKEY")
	return eachObject(ctx, b, args[0], prefix, func(o storage.Object) {
		// %q: una key puede traer saltos de línea o caracteres de control.
		fmt.Fprintf(w, "%d\t%s\t%s\t%q\n", o.Size, o.Modified.Format(time.RFC3339), o.ETag, o.Key)
	})
}

func export(args []string) error {
	if len(args) != 3 {
		return errors.New("uso: chuspa export <chuspa> <key> <archivo>")
	}
	b, err := openForInspection()
	if err != nil {
		return err
	}
	defer b.Close()
	_, content, err := b.GetObject(context.Background(), args[0], args[1])
	if err != nil {
		return err
	}
	defer content.Close()
	// O_EXCL: nunca se sobrescribe un archivo existente.
	out, err := os.OpenFile(args[2], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, content); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func verifyCmd(args []string) error {
	if len(args) != 0 {
		return errors.New("uso: chuspa verify")
	}
	b, err := openForInspection()
	if err != nil {
		return err
	}
	defer b.Close()
	problems := 0
	res, err := b.Verify(context.Background(), func(p fsbolt.Problem) {
		problems++
		fmt.Printf("DAÑADO\t%s\t%q\t%s\n", p.Bucket, p.Key, p.Reason)
	})
	if err != nil {
		return err
	}
	fmt.Printf("verificados %d objetos (%d bytes), %d dañados\n", res.Objects, res.Bytes, problems)
	if problems > 0 {
		return fmt.Errorf("hay %d objetos dañados", problems)
	}
	return nil
}

func rebuildIndex(args []string) error {
	if len(args) != 0 {
		return errors.New("uso: chuspa rebuild-index")
	}
	dir := os.Getenv("CHUSPA_DATA_DIR")
	if dir == "" {
		dir = config.DefaultDataDir
	}
	res, err := fsbolt.Rebuild(dir)
	if err != nil {
		return err
	}
	if res.OldIndex != "" {
		fmt.Printf("índice anterior conservado en %s\n", res.OldIndex)
	}
	fmt.Printf("índice reconstruido: %d chuspas, %d objetos; %d blobs apartados en recuperados/\n",
		res.Buckets, res.Objects, res.Recovered)
	return nil
}

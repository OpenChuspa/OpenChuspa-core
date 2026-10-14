// soak es la prueba de resistencia: varios clientes escriben, leen y borran
// sin parar mientras el motor se mata con SIGKILL a intervalos al azar. Al
// final, todo objeto confirmado debe leerse idéntico y `chuspa verify` no debe
// encontrar daños.
//
// Uso: go run ./test/soak [-duration 2m] [-workers 8]
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	bucket = "aguante"
	region = "cali-san-antonio-1"
	user   = "soak"
)

var (
	duration = flag.Duration("duration", 2*time.Minute, "cuánto dura la prueba")
	workers  = flag.Int("workers", 8, "clientes concurrentes")
	keysPer  = flag.Int("keys", 8, "keys por cliente")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FALLÓ:", err)
		os.Exit(1)
	}
	fmt.Println("Todo bien, mi llave: el motor aguantó.")
}

type motor struct {
	bin, dataDir, addr, secret string
	mu                         sync.Mutex
	cmd                        *exec.Cmd
	log                        *os.File
}

func (m *motor) env() []string {
	return append(os.Environ(), "CHUSPA_ADDR="+m.addr, "CHUSPA_DATA_DIR="+m.dataDir, "CHUSPA_ROOT_USER="+user,
		"CHUSPA_ROOT_PASSWORD="+m.secret, "CHUSPA_LOG_FORMAT=text", "CHUSPA_MIN_FREE_MB=0")
}

func (m *motor) start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := exec.Command(m.bin, "serve")
	cmd.Env, cmd.Stdout, cmd.Stderr = m.env(), m.log, m.log
	if err := cmd.Start(); err != nil {
		return err
	}
	m.cmd = cmd
	for i := 0; i < 100; i++ {
		hc := exec.Command(m.bin, "healthcheck")
		hc.Env = m.env()
		if hc.Run() == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("el motor no arrancó")
}

func (m *motor) stop(sig syscall.Signal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil {
		return
	}
	m.cmd.Process.Signal(sig)
	m.cmd.Wait()
	m.cmd = nil
}

func run() error {
	work, err := os.MkdirTemp("", "chuspa-soak-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	root, err := filepath.Abs(".")
	if err != nil {
		return err
	}
	bin := filepath.Join(work, "chuspa")
	build := exec.Command("go", "build", "-race", "-o", bin, "./cmd/chuspa")
	build.Dir, build.Stdout, build.Stderr = root, os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("compilar: %w", err)
	}
	logFile, err := os.Create(filepath.Join(work, "server.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()

	port, err := freePort()
	if err != nil {
		return err
	}
	m := &motor{bin: bin, dataDir: filepath.Join(work, "data"), addr: fmt.Sprintf("127.0.0.1:%d", port), secret: randHex(24), log: logFile}
	if err := m.start(); err != nil {
		return err
	}
	defer m.stop(syscall.SIGKILL)

	client := s3.New(s3.Options{
		Region: region, BaseEndpoint: aws.String("http://" + m.addr), UsePathStyle: true, RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: user, SecretAccessKey: m.secret}, nil
		}),
	})
	ctx := context.Background()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return err
	}

	deadline := time.Now().Add(*duration)
	var kills atomic.Int64
	chaosDone := make(chan struct{})
	go func() {
		defer close(chaosDone)
		for time.Now().Add(3 * time.Second).Before(deadline) {
			time.Sleep(time.Duration(2000+mrand.IntN(4000)) * time.Millisecond)
			m.stop(syscall.SIGKILL)
			kills.Add(1)
			if err := m.start(); err != nil {
				fmt.Fprintln(os.Stderr, "no se pudo reiniciar el motor:", err)
				os.Exit(1)
			}
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, *workers)
	var ops atomic.Int64
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if err := worker(ctx, client, w, deadline, &ops); err != nil {
				errs <- fmt.Errorf("cliente %d: %w", w, err)
			}
		}(w)
	}
	wg.Wait()
	<-chaosDone
	close(errs)
	for err := range errs {
		return err
	}

	m.stop(syscall.SIGTERM)
	verify := exec.Command(bin, "verify")
	verify.Env = m.env()
	out, err := verify.CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("chuspa verify: %w", err)
	}
	if race, _ := os.ReadFile(logFile.Name()); bytes.Contains(race, []byte("WARNING: DATA RACE")) {
		return errors.New("el detector de carreras encontró una en el motor; ver el log")
	}
	fmt.Printf("%d operaciones confirmadas, %d kill -9\n", ops.Load(), kills.Load())
	return nil
}

// estado de una key desde el punto de vista del cliente que la escribe.
type keyState struct {
	acked   string // hash del contenido confirmado; "" si no existe
	pending string // hash de una escritura sin confirmar; "" si no hay
	deleted bool   // hay un borrado sin confirmar
}

func worker(ctx context.Context, c *s3.Client, w int, deadline time.Time, ops *atomic.Int64) error {
	state := make([]keyState, *keysPer)
	key := func(i int) string { return fmt.Sprintf("cliente-%d/key-%d", w, i) }

	for time.Now().Before(deadline) {
		i := mrand.IntN(len(state))
		st := &state[i]
		// Con una operación sin confirmar, primero se averigua qué quedó.
		if st.pending != "" || st.deleted {
			got, err := read(ctx, c, key(i))
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if got != st.acked && got != st.pending && !(st.deleted && got == "") {
				return fmt.Errorf("%s tras una caída tiene %q; se esperaba %q o %q", key(i), short(got), short(st.acked), short(st.pending))
			}
			st.acked, st.pending, st.deleted = got, "", false
			continue
		}

		var err error
		switch op := mrand.IntN(10); {
		case op < 5:
			data := randBytes(mrand.IntN(256 << 10))
			if mrand.IntN(20) == 0 {
				data = randBytes(6<<20 + mrand.IntN(1<<20))
			}
			st.pending = hash(data)
			if len(data) > 5<<20 {
				err = putMultipart(ctx, c, key(i), data)
			} else {
				_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key(i)), Body: bytes.NewReader(data)})
			}
			if err == nil {
				st.acked, st.pending = st.pending, ""
			}
		case op < 8:
			var got string
			if got, err = read(ctx, c, key(i)); err == nil && got != st.acked {
				return fmt.Errorf("%s tiene %q; se confirmó %q", key(i), short(got), short(st.acked))
			}
		default:
			st.deleted = true
			_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key(i))})
			if err == nil {
				st.acked, st.deleted = "", false
			}
		}
		if err != nil {
			if !transient(err) {
				return fmt.Errorf("%s: %w", key(i), err)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		ops.Add(1)
	}

	// Comprobación final de todas las keys, con el motor ya tranquilo.
	for i := range state {
		for attempt := 0; ; attempt++ {
			got, err := read(ctx, c, key(i))
			if err != nil {
				if attempt > 50 {
					return err
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			st := state[i]
			if got != st.acked && got != st.pending && !(st.deleted && got == "") {
				return fmt.Errorf("al final %s tiene %q; se esperaba %q", key(i), short(got), short(st.acked))
			}
			break
		}
	}
	return nil
}

func putMultipart(ctx context.Context, c *s3.Client, key string, data []byte) error {
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return err
	}
	var parts []types.CompletedPart
	for n, off := int32(1), 0; off < len(data); n, off = n+1, off+5<<20 {
		end := min(off+5<<20, len(data))
		res, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(data[off:end])})
		if err != nil {
			return err
		}
		parts = append(parts, types.CompletedPart{ETag: res.ETag, PartNumber: aws.Int32(n), ChecksumCRC32: res.ChecksumCRC32})
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	return err
}

// read devuelve el hash del contenido, o "" si el objeto no existe.
func read(ctx context.Context, c *s3.Client, key string) (string, error) {
	res, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	h := sha256.New()
	if _, err := io.Copy(h, res.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// transient indica un error esperable mientras el motor está caído.
func transient(err error) bool {
	var netErr net.Error
	var opErr *net.OpError
	msg := err.Error()
	return errors.As(err, &opErr) || errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.Contains(msg, "connection reset") || strings.Contains(msg, "EOF") || strings.Contains(msg, "NoSuchUpload")
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func randHex(n int) string { return hex.EncodeToString(randBytes(n)) }

func short(h string) string {
	if h == "" {
		return "(no existe)"
	}
	return h[:12]
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

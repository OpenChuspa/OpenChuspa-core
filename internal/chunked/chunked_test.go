package chunked

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
)

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Ejemplo de la documentación de AWS ("Signature calculations for the
// Authorization header: transferring a payload in multiple chunks"): 66 560
// bytes de 'a' en chunks de 64 KiB.
func awsExample() (*Signing, string) {
	key := hmacSHA256([]byte("AWS4wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"), "20130524")
	for _, s := range []string{"us-east-1", "s3", "aws4_request"} {
		key = hmacSHA256(key, s)
	}
	signing := &Signing{
		Key:   key,
		Date:  "20130524T000000Z",
		Scope: "20130524/us-east-1/s3/aws4_request",
		Seed:  "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9",
	}
	body := "10000;chunk-signature=ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648\r\n" +
		strings.Repeat("a", 65536) + "\r\n" +
		"400;chunk-signature=0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497\r\n" +
		strings.Repeat("a", 1024) + "\r\n" +
		"0;chunk-signature=b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9\r\n\r\n"
	return signing, body
}

func TestSignedChunksAWSExample(t *testing.T) {
	signing, body := awsExample()
	got, err := io.ReadAll(NewReader(strings.NewReader(body), Options{Signed: true, Signing: signing}))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("a"), 66560)) {
		t.Fatalf("contenido decodificado incorrecto: %d bytes", len(got))
	}
}

func TestSignedChunksTampered(t *testing.T) {
	signing, body := awsExample()
	tampered := strings.Replace(body, "aaaa", "aaab", 1)
	_, err := io.ReadAll(NewReader(strings.NewReader(tampered), Options{Signed: true, Signing: signing}))
	if !errors.Is(err, ErrSignature) {
		t.Fatalf("se esperaba ErrSignature, se obtuvo %v", err)
	}
}

func TestUnsignedTrailer(t *testing.T) {
	body := "5\r\nhola \r\n5\r\nmundo\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	r := NewReader(strings.NewReader(body), Options{Trailer: true})
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hola mundo" {
		t.Fatalf("contenido = %q", got)
	}
	if r.Trailers()["x-amz-checksum-crc32"] != "AAAAAA==" {
		t.Fatalf("trailers = %v", r.Trailers())
	}
}

func TestSignedTrailerRoundTrip(t *testing.T) {
	signing, _ := awsExample()
	data := "oíste mira"
	sum := sha256.Sum256([]byte(data))
	sig1 := signing.sign("AWS4-HMAC-SHA256-PAYLOAD", signing.Seed, emptySHA256, hexString(sum[:]))
	sig2 := signing.sign("AWS4-HMAC-SHA256-PAYLOAD", sig1, emptySHA256, emptySHA256)
	trailer := "x-amz-checksum-crc32:AAAAAA==\n"
	tsum := sha256.Sum256([]byte(trailer))
	sig3 := signing.sign("AWS4-HMAC-SHA256-TRAILER", sig2, hexString(tsum[:]))

	body := "b;chunk-signature=" + sig1 + "\r\n" + data + "\r\n" +
		"0;chunk-signature=" + sig2 + "\r\n" +
		"x-amz-checksum-crc32:AAAAAA==\r\n" +
		"x-amz-trailer-signature:" + sig3 + "\r\n\r\n"
	opts := Options{Signed: true, Trailer: true, Signing: signing}
	if got, err := io.ReadAll(NewReader(strings.NewReader(body), opts)); err != nil || string(got) != data {
		t.Fatalf("got %q, err %v", got, err)
	}

	bad := strings.Replace(body, "AAAAAA==", "AAAAAQ==", 1)
	if _, err := io.ReadAll(NewReader(strings.NewReader(bad), opts)); !errors.Is(err, ErrSignature) {
		t.Fatalf("trailer alterado: se esperaba ErrSignature, se obtuvo %v", err)
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

func TestMalformed(t *testing.T) {
	cases := map[string]struct {
		body string
		opts Options
	}{
		"sin terminador":          {"5\r\nhola ", Options{}},
		"tamaño no hexadecimal":   {"zz\r\n", Options{}},
		"tamaño negativo":         {"-5\r\nhola \r\n0\r\n\r\n", Options{}},
		"falta CRLF tras datos":   {"5\r\nhola XX0\r\n\r\n", Options{}},
		"sin chunk final":         {"5\r\nhola \r\n", Options{}},
		"firma ausente":           {"5\r\nhola \r\n0\r\n\r\n", Options{Signed: true}},
		"firma inesperada":        {"0;chunk-signature=" + strings.Repeat("a", 64) + "\r\n\r\n", Options{}},
		"trailer no permitido":    {"0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n", Options{}},
		"trailer duplicado":       {"0\r\na:1\r\na:2\r\n\r\n", Options{Trailer: true}},
		"solo salto de línea":     {"5\nhola \n0\n\n", Options{}},
		"línea demasiado larga":   {strings.Repeat("1", maxLineLen+10) + "\r\n", Options{}},
		"falta firma del trailer": {"0;chunk-signature=" + strings.Repeat("a", 64) + "\r\na:1\r\n\r\n", Options{Signed: true, Trailer: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := io.ReadAll(NewReader(strings.NewReader(tc.body), tc.opts)); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}

func FuzzReader(f *testing.F) {
	_, example := awsExample()
	f.Add([]byte(example), true, false)
	f.Add([]byte("5\r\nhola \r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"), false, true)
	f.Add([]byte("0\r\n\r\n"), false, false)
	f.Fuzz(func(t *testing.T, body []byte, signed, trailer bool) {
		r := NewReader(bytes.NewReader(body), Options{Signed: signed, Trailer: trailer})
		n, _ := io.Copy(io.Discard, r)
		if n > int64(len(body)) {
			t.Fatalf("se decodificaron %d bytes de una entrada de %d", n, len(body))
		}
	})
}

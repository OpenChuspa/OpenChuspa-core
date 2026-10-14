package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	testKey    = "parcero"
	testSecret = "0123456789abcdef0123456789abcdef"
	testRegion = "cali-san-antonio-1"
	emptyHash  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var testTime = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func verifier() *Verifier {
	return &Verifier{AccessKey: testKey, SecretKey: testSecret, Region: testRegion, Now: func() time.Time { return testTime }}
}

// signed arma una petición y la firma con el firmador oficial del SDK.
func signed(t *testing.T, method, target, secret, payloadHash string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, "http://localhost:9000"+target, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	creds := aws.Credentials{AccessKeyID: testKey, SecretAccessKey: secret}
	err := v4.NewSigner().SignHTTP(context.Background(), creds, r, payloadHash, "s3", testRegion, testTime,
		func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVerifyAgainstOfficialSigner(t *testing.T) {
	targets := []struct{ method, target string }{
		{"GET", "/"},
		{"PUT", "/feria-de-cali"},
		{"GET", "/feria-de-cali?list-type=2&prefix=test%2F&delimiter=%2F&encoding-type=url"},
		{"PUT", "/feria-de-cali/test/salsa.txt"},
		{"PUT", "/feria-de-cali/con%20espacio/y%2Bm%C3%A1s%21%28raro%29%2A.txt"},
		{"POST", "/feria-de-cali/grande.bin?uploads"},
		{"PUT", "/feria-de-cali/grande.bin?partNumber=2&uploadId=abc"},
		{"GET", "/feria-de-cali/..%2F..%2Fetc%2Fpasswd"},
	}
	for _, tc := range targets {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			r := signed(t, tc.method, tc.target, testSecret, emptyHash, map[string]string{
				"Content-Type": "text/plain", "Range": "bytes=0-9", "X-Amz-Meta-Barrio": "  San   Antonio ",
			})
			id, err := verifier().Verify(r)
			if err != nil {
				t.Fatalf("la firma oficial no se verificó: %v", err)
			}
			if id.Signing == nil || id.Signing.Seed == "" {
				t.Fatal("falta el contexto de firma para los chunks")
			}
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(r *http.Request, v *Verifier)
		want   error
	}{
		{"secreto incorrecto", func(r *http.Request, v *Verifier) { v.SecretKey = strings.Repeat("x", 32) }, ErrSignature},
		{"llave desconocida", func(r *http.Request, v *Verifier) { v.AccessKey = "otro" }, ErrSignature},
		{"otra región", func(r *http.Request, v *Verifier) { v.Region = "us-east-1" }, ErrSignature},
		{"sin firma", func(r *http.Request, v *Verifier) { r.Header.Del("Authorization") }, ErrMissing},
		{"header firmado alterado", func(r *http.Request, v *Verifier) { r.Header.Set("Content-Type", "text/html") }, ErrSignature},
		{"hash del cuerpo alterado", func(r *http.Request, v *Verifier) { r.Header.Set("X-Amz-Content-Sha256", UnsignedPayload) }, ErrSignature},
		{"ruta alterada", func(r *http.Request, v *Verifier) { r.URL.Path = "/feria-de-cali/otro.txt" }, ErrSignature},
		{"query alterada", func(r *http.Request, v *Verifier) { r.URL.RawQuery = "acl" }, ErrSignature},
		{"método alterado", func(r *http.Request, v *Verifier) { r.Method = "DELETE" }, ErrSignature},
		{"reloj adelantado", func(r *http.Request, v *Verifier) {
			v.Now = func() time.Time { return testTime.Add(16 * time.Minute) }
		}, ErrSkew},
		{"reloj atrasado", func(r *http.Request, v *Verifier) {
			v.Now = func() time.Time { return testTime.Add(-16 * time.Minute) }
		}, ErrSkew},
		{"prefirmada", func(r *http.Request, v *Verifier) { r.URL.RawQuery = "X-Amz-Signature=abc" }, ErrUnsupported},
		{"SigV2", func(r *http.Request, v *Verifier) { r.Header.Set("Authorization", "AWS parcero:abc") }, ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := signed(t, "PUT", "/feria-de-cali/test/salsa.txt", testSecret, emptyHash, map[string]string{"Content-Type": "text/plain"})
			v := verifier()
			tt.mutate(r, v)
			if _, err := v.Verify(r); !errors.Is(err, tt.want) {
				t.Fatalf("se esperaba %v, se obtuvo %v", tt.want, err)
			}
		})
	}
}

func TestRequiredSignedHeaders(t *testing.T) {
	r := signed(t, "GET", "/", testSecret, emptyHash, nil)
	auth := strings.Replace(r.Header.Get("Authorization"), "host;", "", 1)
	r.Header.Set("Authorization", auth)
	if _, err := verifier().Verify(r); !errors.Is(err, ErrSignature) {
		t.Fatalf("sin host firmado se esperaba ErrSignature, se obtuvo %v", err)
	}
}

func TestParsePayload(t *testing.T) {
	for _, v := range []string{"", UnsignedPayload, StreamingSigned, StreamingSignedTrailer, StreamingUnsignedTrailer, emptyHash} {
		if _, err := ParsePayload(v); err != nil {
			t.Errorf("ParsePayload(%q): %v", v, err)
		}
	}
	for _, v := range []string{"STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD", "abc", strings.ToUpper(emptyHash)} {
		if _, err := ParsePayload(v); err == nil {
			t.Errorf("ParsePayload(%q) debería fallar", v)
		}
	}
}

func FuzzParseAuthorization(f *testing.F) {
	f.Add("AWS4-HMAC-SHA256 Credential=parcero/20261013/cali-san-antonio-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=" + emptyHash)
	f.Add("AWS4-HMAC-SHA256 Credential=,SignedHeaders=,Signature=")
	f.Add("")
	f.Fuzz(func(t *testing.T, header string) {
		a, err := ParseAuthorization(header)
		if err == nil && (a.AccessKey == "" || len(a.SignedHeaders) == 0 || len(a.Signature) != 64) {
			t.Fatalf("se aceptó un header incompleto: %q", header)
		}
	})
}

func TestRotacionDelSecreto(t *testing.T) {
	const nuevo = "fedcba9876543210fedcba9876543210"
	v := verifier()
	v.SecretKey, v.PreviousSecretKey = nuevo, testSecret

	for _, secret := range []string{nuevo, testSecret} {
		id, err := v.Verify(signed(t, "GET", "/", secret, emptyHash, nil))
		if err != nil {
			t.Fatalf("durante la rotación se rechazó un secreto válido: %v", err)
		}
		// La llave de los chunks debe ser la del secreto que firmó.
		if want := DeriveKey(secret, "20261013", testRegion, "s3"); string(id.Signing.Key) != string(want) {
			t.Fatal("la llave de firma de los chunks no corresponde al secreto usado")
		}
	}
	if _, err := v.Verify(signed(t, "GET", "/", "0000000000000000000000000000000000", emptyHash, nil)); err == nil {
		t.Fatal("se aceptó un secreto que no es ni el nuevo ni el anterior")
	}
	v.PreviousSecretKey = ""
	if _, err := v.Verify(signed(t, "GET", "/", testSecret, emptyHash, nil)); err == nil {
		t.Fatal("terminada la rotación se siguió aceptando el secreto anterior")
	}
}

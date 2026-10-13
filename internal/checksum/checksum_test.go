package checksum

import (
	"encoding/hex"
	"testing"
)

// Valores de comprobación estándar de cada algoritmo para "123456789".
func TestCheckValues(t *testing.T) {
	want := map[string]string{
		CRC32:     "cbf43926",
		CRC32C:    "e3069283",
		CRC64NVME: "ae8b14860a799888",
		SHA1:      "f7c3bc1d808e04732adf679965ccc34ca7ae3441",
		SHA256:    "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225",
	}
	for _, algo := range Algorithms {
		h := New(algo)
		h.Write([]byte("123456789"))
		if got := hex.EncodeToString(h.Sum(nil)); got != want[algo] {
			t.Errorf("%s: se obtuvo %s, se esperaba %s", algo, got, want[algo])
		}
	}
}

func TestNormalize(t *testing.T) {
	if a, ok := Normalize("CRC64NVME"); !ok || a != CRC64NVME {
		t.Fatalf("Normalize(CRC64NVME) = %q, %v", a, ok)
	}
	if _, ok := Normalize("md5"); ok {
		t.Fatal("md5 no debería estar soportado como checksum")
	}
}

func TestComposite(t *testing.T) {
	got, ok := Composite(CRC32, []string{"y/Q5Jg==", "y/Q5Jg=="})
	if !ok || len(got) < 3 || got[len(got)-2:] != "-2" {
		t.Fatalf("Composite = %q, %v", got, ok)
	}
	if _, ok := Composite(CRC32, []string{"no es base64"}); ok {
		t.Fatal("se esperaba rechazo de base64 inválido")
	}
}

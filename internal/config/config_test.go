package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodSecret = "0123456789abcdef0123456789abcdef"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"sin credenciales no arranca", map[string]string{}, "faltan credenciales"},
		{"solo usuario", map[string]string{"CHUSPA_ROOT_USER": "parcero"}, "faltan credenciales"},
		{"secreto corto", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": "corto"}, "muy corto"},
		{"válida en loopback", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret}, ""},
		{"válida expuesta", map[string]string{"CHUSPA_ADDR": ":9000", "CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret}, ""},
		{"credencial de ejemplo expuesta", map[string]string{"CHUSPA_ADDR": "0.0.0.0:9000", "CHUSPA_ROOT_USER": "clave-del-parchao", "CHUSPA_ROOT_PASSWORD": goodSecret}, "credenciales de ejemplo"},
		{"credencial de ejemplo en loopback", map[string]string{"CHUSPA_ROOT_USER": "clave-del-parchao", "CHUSPA_ROOT_PASSWORD": goodSecret}, ""},
		{"anónimo en loopback", map[string]string{"CHUSPA_ALLOW_ANONYMOUS": "true"}, ""},
		{"anónimo expuesto", map[string]string{"CHUSPA_ADDR": ":9000", "CHUSPA_ALLOW_ANONYMOUS": "true"}, "solo se permite escuchando en loopback"},
		{"usuario con barra", map[string]string{"CHUSPA_ROOT_USER": "a/b", "CHUSPA_ROOT_PASSWORD": goodSecret}, "CHUSPA_ROOT_USER inválido"},
		{"dirección inválida", map[string]string{"CHUSPA_ADDR": "9000"}, "CHUSPA_ADDR inválida"},
		{"expiración inválida", map[string]string{"CHUSPA_MULTIPART_EXPIRY": "una semana"}, "CHUSPA_MULTIPART_EXPIRY inválida"},
		{"dos fuentes del secreto", map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD": goodSecret, "CHUSPA_ROOT_PASSWORD_FILE": "/x"}, "no ambas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("error inesperado: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("se esperaba un error con %q, se obtuvo: %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadSecretFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secreto")
	if err := os.WriteFile(path, []byte(goodSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{"CHUSPA_ROOT_USER": "parcero", "CHUSPA_ROOT_PASSWORD_FILE": path}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RootPassword != goodSecret {
		t.Fatalf("el secreto leído del archivo no coincide")
	}
}

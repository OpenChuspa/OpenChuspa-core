package main

import (
	"path"
	"strings"
)

// location es una dirección chuspa://chuspa/key. También se acepta s3://.
type location struct {
	Bucket string
	Key    string
}

func (l location) String() string { return "chuspa://" + l.Bucket + "/" + l.Key }

// isRemote indica si el argumento es una dirección de chuspa.
func isRemote(s string) bool {
	return strings.HasPrefix(s, "chuspa://") || strings.HasPrefix(s, "s3://")
}

func parseLocation(s string) (location, error) {
	rest, ok := strings.CutPrefix(s, "chuspa://")
	if !ok {
		if rest, ok = strings.CutPrefix(s, "s3://"); !ok {
			return location{}, usagef("%q no es una dirección de chuspa: se espera chuspa://chuspa/key", s)
		}
	}
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" && key != "" {
		return location{}, usagef("%q no tiene nombre de chuspa", s)
	}
	return location{Bucket: bucket, Key: key}, nil
}

// uploadKey decide la key de un archivo subido a dst: si dst es la raíz de la
// chuspa o termina en "/", se le agrega el nombre del archivo.
func uploadKey(dst location, file string) string {
	if dst.Key == "" || strings.HasSuffix(dst.Key, "/") {
		return dst.Key + path.Base(strings.ReplaceAll(file, "\\", "/"))
	}
	return dst.Key
}

// dropConnector quita el "a" opcional antes del destino: "ve subi x a chuspa://...".
func dropConnector(args []string) []string {
	if len(args) >= 3 && args[len(args)-2] == "a" {
		return append(args[:len(args)-2:len(args)-2], args[len(args)-1])
	}
	return args
}

// splitFlags separa las opciones conocidas de los argumentos posicionales.
func splitFlags(args []string, known ...string) (pos []string, flags map[string]bool, err error) {
	flags = map[string]bool{}
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			matched := false
			for _, k := range known {
				if a == k {
					flags[k], matched = true, true
				}
			}
			if !matched {
				return nil, nil, usagef("opción desconocida %q", a)
			}
			continue
		}
		pos = append(pos, a)
	}
	return pos, flags, nil
}

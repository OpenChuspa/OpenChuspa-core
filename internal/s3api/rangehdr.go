package s3api

import (
	"strconv"
	"strings"
)

// parseRange interpreta un header Range de un solo rango de bytes sobre un
// objeto de size bytes. ok=false significa "sin rango": se sirve el objeto
// completo (header ausente, mal formado o con varios rangos, igual que S3).
// satisfiable=false corresponde a un 416.
func parseRange(h string, size int64) (start, length int64, ok, satisfiable bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, true
	}
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false, true
	}
	if first == "" { // sufijo: los últimos N bytes
		n, err := parseOffset(last)
		if err != nil {
			return 0, 0, false, true
		}
		if n == 0 || size == 0 {
			return 0, 0, true, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true, true
	}
	a, err := parseOffset(first)
	if err != nil {
		return 0, 0, false, true
	}
	if a >= size {
		return 0, 0, true, false
	}
	b := size - 1
	if last != "" {
		v, err := parseOffset(last)
		if err != nil || v < a {
			return 0, 0, false, true
		}
		if v < b {
			b = v
		}
	}
	return a, b - a + 1, true, true
}

func parseOffset(s string) (int64, error) {
	if s == "" || len(s) > 19 || s[0] == '+' || s[0] == '-' {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseInt(s, 10, 64)
}

package dataset

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func NormalizeName(s string) string {
	s = norm.NFKC.String(s)
	s = cases.Fold().String(s)
	s = strings.TrimSpace(s)
	fields := strings.FieldsFunc(s, unicode.IsSpace)
	return strings.Join(fields, " ")
}

func SectorFromAffiliation(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '；' })
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.ContainsAny(part, "()（）") {
			continue
		}
		return part
	}
	return "其他"
}

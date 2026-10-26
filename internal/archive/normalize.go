package archive

import (
	"errors"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// letters that Unicode does not decompose into base letter + mark.
var foldExtra = map[rune]string{
	'ł': "l", 'đ': "d", 'ð': "d", 'ø': "o", 'æ': "ae", 'œ': "oe",
	'ß': "ss", 'ı': "i", 'þ': "th", 'ħ': "h", 'ŀ': "l",
}

// SearchText folds text for indexing and matching: lowercase, no diacritics,
// so that "lodz" finds "Łódź" and "zolw" finds "żółw".
func SearchText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if rep, ok := foldExtra[r]; ok {
			b.WriteString(rep)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func searchTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// MatchQuery turns free text into an FTS5 MATCH expression. Every word must
// appear (as a prefix); text in "double quotes" must appear as an exact phrase.
func MatchQuery(q string) (string, error) {
	var parts []string
	for {
		i := strings.IndexByte(q, '"')
		if i < 0 {
			break
		}
		j := strings.IndexByte(q[i+1:], '"')
		if j < 0 {
			q = q[:i] + " " + q[i+1:]
			break
		}
		if toks := searchTokens(SearchText(q[i+1 : i+1+j])); len(toks) > 0 {
			parts = append(parts, `"`+strings.Join(toks, " ")+`"`)
		}
		q = q[:i] + " " + q[i+2+j:]
	}
	for _, t := range searchTokens(SearchText(q)) {
		parts = append(parts, `"`+t+`"*`)
	}
	if len(parts) == 0 {
		return "", errors.New("the search query has no searchable words")
	}
	return strings.Join(parts, " "), nil
}

// ContainsFold reports whether needle occurs in haystack, ignoring case and diacritics.
func ContainsFold(haystack, needle string) bool {
	return strings.Contains(SearchText(haystack), SearchText(needle))
}

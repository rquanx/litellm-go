package runtime

import (
	"slices"
	"testing"
)

func TestRAGDocumentSplitterMatchesLiteLLM(t *testing.T) {
	for _, test := range []struct {
		input                string
		size, overlap        int
		separators, expected []string
	}{
		{"one two three four five", 8, 2, nil, []string{"one two", "two three", "three four", "four five"}},
		{"一二三|四五六七八|九", 4, 1, []string{"|", ""}, []string{"一二三", "四五六七", "七八", "九"}},
		{"abcdefghijk", 6, 2, []string{";"}, []string{"abcdef", "efghij", "ijk"}},
	} {
		if chunks := splitRAGDocument(test.input, test.size, test.overlap, test.separators); !slices.Equal(chunks, test.expected) {
			t.Errorf("split %q into %q, expected %q", test.input, chunks, test.expected)
		}
	}
}

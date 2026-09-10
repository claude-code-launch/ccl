package oauthproxy

import (
	"strings"
	"testing"

	"github.com/tiktoken-go/tokenizer"
)

func TestConcreteCodexTokenizerMatchesReference(t *testing.T) {
	reference, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		"", "Hello, world!", "中文、日本語、한국어。🚀👨‍👩‍👧‍👦",
		`{"input":[{"type":"function_call","arguments":"{\"id\":9007199254740993}"}],"tools":[]}`,
		"func main() {\n\tfmt.Println(\"hello\")\n}\n",
		"<|endoftext|> <|endofprompt|> 0123456789\r\n\t  ",
		strings.Repeat("a_long_identifier = 0.123456789; // 测试\n", 100),
	} {
		got, err := countCodexResponsesInputTokens([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		want, err := reference.Count(input)
		if err != nil || got != want {
			t.Fatalf("count=%d, reference=%d, error=%v", got, want, err)
		}
	}
}

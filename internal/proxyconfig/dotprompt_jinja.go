package proxyconfig

import (
	"errors"
	"strings"

	"github.com/nikolalohinski/gonja/v2/builtins"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/nikolalohinski/gonja/v2/parser"
	"github.com/nikolalohinski/gonja/v2/tokens"
)

var dotpromptJinjaConfig = func() *config.Config {
	cfg := config.New()
	cfg.AutoEscape = true
	return cfg
}()

var dotpromptJinjaEnv = func() *exec.Environment {
	structures := make(map[string]parser.ControlStructureParser)
	for _, name := range []string{"if", "for", "raw", "autoescape"} {
		structures[name], _ = builtins.ControlStructures.Get(name)
	}
	filters := make(map[string]exec.FilterFunction)
	for _, name := range []string{"capitalize", "default", "d", "e", "escape", "first", "int", "last", "length", "list", "lower", "safe", "string", "trim", "upper"} {
		if fn, ok := builtins.Filters.Get(name); ok {
			filters[name] = fn
		}
	}
	return &exec.Environment{Context: exec.EmptyContext(), Filters: exec.NewFilterSet(filters), Tests: builtins.Tests,
		ControlStructures: exec.NewControlStructureSet(structures)}
}()

func renderDotpromptJinja(source string, variables map[string]any) (string, error) {
	if len(source) > 64<<10 || !validDotpromptValues(variables, 0) {
		return "", errors.New("Prompt exceeds the template limits")
	}
	if err := validateDotpromptJinja(source); err != nil {
		return "", err
	}
	const filename = "/prompt"
	loader := loaders.MustNewMemoryLoader(map[string]string{filename: source})
	template, err := exec.NewTemplate(filename, dotpromptJinjaConfig, loader, dotpromptJinjaEnv)
	if err != nil {
		return "", errors.New("Invalid or unsupported Jinja template")
	}
	output := limitedPromptWriter{}
	if err := template.Execute(&output, exec.NewContext(variables)); err != nil {
		return "", errors.New("Could not render Jinja template")
	}
	return output.String(), nil
}

func validDotpromptValues(value any, depth int) bool {
	if depth > 12 {
		return false
	}
	switch value := value.(type) {
	case []any:
		if len(value) > 256 {
			return false
		}
		for _, item := range value {
			if !validDotpromptValues(item, depth+1) {
				return false
			}
		}
	case map[string]any:
		if len(value) > 256 {
			return false
		}
		for _, item := range value {
			if !validDotpromptValues(item, depth+1) {
				return false
			}
		}
	case string:
		return len(value) <= 4<<10
	}
	return true
}

func validateDotpromptJinja(source string) error {
	stream := tokens.LexAll(source, dotpromptJinjaConfig)
	blockDepth, loopDepth, expressionTokens := 0, 0, 0
	totalTokens, loops := 0, 0
	previous := tokens.Initial
	for !stream.End() {
		token := stream.Next()
		totalTokens++
		switch token.Type {
		case tokens.BlockBegin, tokens.VariableBegin:
			expressionTokens = 0
			if token.Type == tokens.BlockBegin && stream.Current() != nil {
				switch stream.Current().Val {
				case "for":
					loops++
					loopDepth++
					blockDepth++
				case "endfor":
					loopDepth--
					blockDepth--
				case "if", "autoescape":
					blockDepth++
				case "endif", "endautoescape":
					blockDepth--
				}
			}
		case tokens.Data, tokens.Comment:
		default:
			expressionTokens++
		}
		if expressionTokens > 64 || totalTokens > 2048 || blockDepth > 12 || loopDepth > 1 || loops > 8 ||
			(token.Type == tokens.String && len(token.Val) > 4<<10) ||
			token.Type == tokens.Multiply || token.Type == tokens.Power ||
			(token.Type == tokens.Name && (token.Val == "recursive" || strings.HasPrefix(token.Val, "_"))) ||
			(token.Type == tokens.LeftParenthesis && (previous == tokens.Name || previous == tokens.RightBracket || previous == tokens.RightParenthesis)) {
			return errors.New("Unsupported Jinja expression or nesting")
		}
		previous = token.Type
	}
	if stream.IsError() {
		return errors.New("Invalid Jinja expression")
	}
	return nil
}

type limitedPromptWriter struct{ strings.Builder }

func (writer *limitedPromptWriter) Write(data []byte) (int, error) {
	if len(data) > 2<<20-writer.Len() {
		return 0, errors.New("Rendered prompt exceeds the size limit")
	}
	return writer.Builder.Write(data)
}

func (writer *limitedPromptWriter) WriteString(data string) (int, error) {
	if len(data) > 2<<20-writer.Len() {
		return 0, errors.New("Rendered prompt exceeds the size limit")
	}
	return writer.Builder.WriteString(data)
}

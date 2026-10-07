package tools

import (
	"fmt"
	"go/constant"
	"go/token"
	"go/types"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type calculateArgs struct {
	Expression string `json:"expression" jsonschema:"arithmetic expression using + - * / % and parentheses, e.g. (2+3)*4/7"`
}

// calculate evaluates a constant arithmetic expression with Go's constant
// evaluator, so there is no parser to maintain and nothing can be executed.
func calculate(Deps) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "calculate",
		Description: "Evaluate an arithmetic expression exactly.",
	}, func(_ adkagent.Context, a calculateArgs) (map[string]any, error) {
		tv, err := types.Eval(token.NewFileSet(), nil, token.NoPos, a.Expression)
		if err != nil {
			return nil, fmt.Errorf("invalid expression: %w", err)
		}
		if tv.Value == nil {
			return nil, fmt.Errorf("not a constant expression")
		}
		f, _ := constant.Float64Val(constant.ToFloat(tv.Value))
		return map[string]any{"expression": a.Expression, "result": f}, nil
	})
}

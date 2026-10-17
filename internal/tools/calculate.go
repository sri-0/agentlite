package tools

import (
	"errors"
	"fmt"
	"math"

	"github.com/expr-lang/expr"

	"agentlite/pkg/toolx"
)

type calculateArgs struct {
	Expression string `json:"expression" jsonschema:"math expression, e.g. (2+3)*4/7, 2^10, sqrt(2)*pi, max(3, 7.5)"`
}

// mathEnv is all an expression can call beyond expr's own built-ins (abs,
// ceil, floor, round, min, max): no I/O, nothing else runs.
var mathEnv = map[string]any{
	"sqrt": math.Sqrt, "log": math.Log, "log10": math.Log10, "exp": math.Exp,
	"sin": math.Sin, "cos": math.Cos, "tan": math.Tan, "pi": math.Pi, "e": math.E,
}

// calculate evaluates a math expression with expr-lang/expr: decimal
// division, powers, remainders, comparisons and the functions above.
func calculate(toolx.Connector) (toolx.Tool, error) {
	return toolx.New(toolx.Spec{
		Name: "calculate",
		Description: "Evaluate a math expression: + - * / % ^, parentheses, comparisons, " +
			"abs ceil floor round min max sqrt log log10 exp sin cos tan, pi and e.",
	}, func(_ toolx.Context, a calculateArgs) (map[string]any, error) {
		program, err := expr.Compile(a.Expression, expr.Env(mathEnv)) // typed: sqrt(2) takes the 2 as a float
		if err != nil {
			return nil, fmt.Errorf("invalid expression: %w", err)
		}
		result, err := expr.Run(program, mathEnv)
		if err != nil {
			return nil, err
		}
		if f, ok := result.(float64); ok && (math.IsInf(f, 0) || math.IsNaN(f)) {
			return nil, errors.New("the result is not a finite number (division by zero?)")
		}
		return map[string]any{"expression": a.Expression, "result": result}, nil
	})
}

package builtin

import (
	"fmt"
	"slices"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"agentlite/internal/stream"
)

type todo struct {
	ID      string `json:"id" jsonschema:"stable id for the task"`
	Content string `json:"content" jsonschema:"what the task is"`
	Status  string `json:"status" jsonschema:"pending, in_progress, completed or cancelled"`
}

type todoWriteArgs struct {
	Todos []todo `json:"todos" jsonschema:"the complete current task list"`
}

var todoStatuses = []string{"pending", "in_progress", "completed", "cancelled"}

// todoWrite records the agent's task list; the encoder turns the result into
// a task_list activity the UI renders.
func todoWrite(Deps) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name: stream.ToolTodoWrite,
		Description: "Replace your task list with the complete current list. Use it for work with three or more steps: " +
			"write it before starting, keep exactly the tasks being worked on in_progress, mark a task completed as soon as it is " +
			"actually done (never on intent), and cancelled if it was dropped or failed.",
	}, func(_ adkagent.Context, a todoWriteArgs) (map[string]any, error) {
		tasks := make([]map[string]any, 0, len(a.Todos))
		for _, t := range a.Todos {
			if !slices.Contains(todoStatuses, t.Status) {
				return nil, fmt.Errorf("task %q: status must be one of %v", t.ID, todoStatuses)
			}
			tasks = append(tasks, map[string]any{"id": t.ID, "title": t.Content, "status": t.Status})
		}
		return map[string]any{"tasks": tasks}, nil
	})
}

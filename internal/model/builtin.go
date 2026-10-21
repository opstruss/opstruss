package model

import (
	_ "embed"
	"fmt"
	"sync"
)

//go:embed builtin.yaml
var builtinYAML []byte

// builtins are the inputs and outputs of the built-in tasks, by name.
var builtins = sync.OnceValue(func() map[string]*Task {
	doc, err := parseYAML(builtinYAML, "builtin.yaml")
	if err != nil {
		panic(fmt.Sprintf("builtin.yaml: %v", err))
	}
	tasks := map[string]*Task{}
	if err := decodeValue(doc.value, &tasks); err != nil {
		panic(fmt.Sprintf("builtin.yaml: %v", err))
	}
	return tasks
})

// Builtin returns the inputs and outputs of the built-in task name.
func Builtin(name string) *Task { return builtins()[name] }

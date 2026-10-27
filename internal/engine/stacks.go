package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/state"
)

// undeclaredReaches are the commands of the engine that reach a stack the
// project no longer declares: destroy takes its records down, and logs
// reads them; opstruss state reads and repairs them without the engine.
var undeclaredReaches = []string{"destroy", "logs"}

// undeclaredError refuses command on a stack that the project no longer
// declares.
func undeclaredError(p *model.Project) error {
	names, _ := model.Stacks(p.Dir)
	return fmt.Errorf("the project has no stack %s; it has %s. The records of %s stay: opstruss destroy --stack %s takes them down",
		p.StackName, strings.Join(names, ", "), p.StackName, p.StackName)
}

// undeclaredStacks says, for each stack with records that the project no
// longer declares, how many records it holds, as plan, up, and doctor warn.
func (e *engine) undeclaredStacks() []string {
	names, err := model.Stacks(e.p.Dir)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(e.root, "state"))
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range entries {
		stack := d.Name()
		if !d.IsDir() || slices.Contains(names, stack) {
			continue
		}
		st := &state.Store{Backend: e.store.Backend, Stack: stack}
		addrs, err := st.Inventory()
		if err != nil {
			continue
		}
		n := 0
		for _, a := range addrs {
			if r, _, err := st.Get(a); err == nil && r != nil && r.Tombstone == nil {
				n++
			}
		}
		if n == 0 {
			continue
		}
		records := fmt.Sprintf("%d records", n)
		if n == 1 {
			records = "1 record"
		}
		out = append(out, fmt.Sprintf("stack %s has %s, but the project does not declare it; opstruss destroy --stack %s takes them down",
			stack, records, stack))
	}
	return out
}

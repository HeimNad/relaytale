// template-version validates a local template and emits content-addressed
// startup configuration. It never contacts RelayTale or publishes a version.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"relaytale/internal/api"
)

func main() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (32<<10)+1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read template")
		os.Exit(1)
	}
	t, err := api.VersionTemplateJSON(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid template or version")
		os.Exit(1)
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err = e.Encode(t); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write template")
		os.Exit(1)
	}
}

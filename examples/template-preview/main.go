// Local preview emits JSON source only; it never sends mail or opens a browser.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"relaytale/internal/api"
)

func main() {
	path := flag.String("template", "", "local template JSON file")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "use -template FILE and variables JSON on stdin")
		os.Exit(1)
	}
	f, err := os.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open template")
		os.Exit(1)
	}
	defer f.Close()
	source, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read template")
		os.Exit(1)
	}
	variables, err := io.ReadAll(io.LimitReader(os.Stdin, (16<<10)+1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read variables")
		os.Exit(1)
	}
	preview, err := api.PreviewTemplateJSON(source, variables)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid template, version or variables")
		os.Exit(1)
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err = e.Encode(preview); err != nil {
		fmt.Fprintln(os.Stderr, "cannot write preview")
		os.Exit(1)
	}
}

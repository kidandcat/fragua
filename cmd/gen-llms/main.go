// Command gen-llms writes docs/llms.txt and docs/llms-full.txt.
//
//	go run ./cmd/gen-llms [docs-dir]
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mentasystems/fragua/internal/llms"
)

func main() {
	dir := "docs"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	for name, content := range map[string]string{
		"llms.txt":      llms.Index(),
		"llms-full.txt": llms.Full(),
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "gen-llms: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("wrote", path)
	}
}

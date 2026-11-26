// Command rendertmpl prints a built-in Dockerfile template to stdout.
//
//	go run ./scripts/rendertmpl <template> [key=value ...]
//
// It exists for scripts/smoke-templates.sh, which builds each template's output
// with real docker. Rendering is the only part of that check that needs to be
// inside the module, so this stays as small as it can be.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/qooqapp/qooq/internal/watcher"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: rendertmpl <template> [key=value ...]\navailable: %s\n",
			strings.Join(watcher.BuiltinTemplateNames(), ", "))
		os.Exit(2)
	}
	vars := map[string]string{}
	for _, arg := range os.Args[2:] {
		// The caller passes an empty string when it has no overrides, which is
		// simpler on its side than building a variadic argument list.
		if strings.TrimSpace(arg) == "" {
			continue
		}
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "not a key=value: %q\n", arg)
			os.Exit(2)
		}
		vars[k] = v
	}
	out, err := watcher.RenderBuiltinDockerfile(os.Args[1], vars)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(out)
}

package watcher

import (
	"sort"
	"strings"
	"testing"
	"text/template/parse"
)

// A misspelled variable used to be dropped: static_dir written as dir built
// an image that served nginx's welcome page, and the deploy reported success.
func TestRenderDockerfileRejectsUnknownVariable(t *testing.T) {
	_, err := renderDockerfile("static-nginx", map[string]string{"dir": "public", "static_dir": "public"}, "")
	if err == nil {
		t.Fatal("rendered with an unknown template variable")
	}
	for _, want := range []string{`"dir"`, "static-nginx", "static_dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
	if _, err := renderDockerfile("static-nginx", map[string]string{"static_dir": "public"}, ""); err != nil {
		t.Errorf("a valid variable was rejected: %v", err)
	}
}

// The defaults table is the list of valid names, so it has to be exactly the
// set each template reads: a variable missing from it would be rejected even
// though the template uses it, and an extra one would be accepted and ignored.
// Also the variables pkg/manifest's detection writes must all be valid.
func TestTemplateDefaultsMatchWhatTemplatesRead(t *testing.T) {
	for name, defaults := range templateDefaults {
		body, err := templateFS.ReadFile("templates/" + name + "/Dockerfile.tmpl")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		trees, err := parse.Parse("t", string(body), "{{", "}}")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		used := map[string]bool{}
		for _, tr := range trees {
			collectFields(tr.Root, used)
		}
		var missing, extra []string
		for k := range used {
			if _, ok := defaults[k]; !ok {
				missing = append(missing, k)
			}
		}
		for k := range defaults {
			if !used[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 {
			t.Errorf("%s: template reads %v but has no default for it", name, missing)
		}
		if len(extra) > 0 {
			t.Errorf("%s: default %v is never read by the template", name, extra)
		}
	}
}

func collectFields(n parse.Node, into map[string]bool) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			collectFields(c, into)
		}
	case *parse.ActionNode:
		collectFields(n.Pipe, into)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, c := range n.Cmds {
			collectFields(c, into)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			collectFields(a, into)
		}
	case *parse.FieldNode:
		into[n.Ident[0]] = true
	case *parse.IfNode:
		collectFields(n.Pipe, into)
		collectFields(n.List, into)
		collectFields(n.ElseList, into)
	case *parse.RangeNode:
		collectFields(n.Pipe, into)
		collectFields(n.List, into)
		collectFields(n.ElseList, into)
	case *parse.WithNode:
		collectFields(n.Pipe, into)
		collectFields(n.List, into)
		collectFields(n.ElseList, into)
	}
}

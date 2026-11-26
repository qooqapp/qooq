package core

import (
	"errors"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

// The default namespace must not qualify the container name or the alias.
// Those are what an already-running container is addressed by — its ingress
// route, its siblings' URLs — and prefixing them would break every one of them
// until the component happened to be recreated.
func TestNamingDefaultNamespaceDoesNotQualify(t *testing.T) {
	spec := ComponentSpec{Namespace: client.DefaultNamespace, Name: "api"}

	if got := ContainerName(spec); got != "api" {
		t.Errorf("ContainerName = %q, want the bare name", got)
	}
	if got := networkAliases(spec); len(got) != 1 || got[0] != "api" {
		t.Errorf("networkAliases = %v, want just the bare name", got)
	}
	// Display is the exception: it is always qualified, because a bare name
	// stops identifying anything once a second namespace takes it.
	if got := displayName(spec); got != "default/api" {
		t.Errorf("displayName = %q, want default/api", got)
	}
	if err := validateNaming(spec); err != nil {
		t.Errorf("a valid default-namespace name was rejected: %v", err)
	}
}

// The DNS-label rule now applies in the default namespace too. It used to be
// waived there, back when an empty namespace meant "predates namespaces"; the
// reason for the rule was never the namespace, it is that the name becomes a
// DNS alias its siblings resolve.
func TestNamingRejectsNonLabelNameInDefaultNamespace(t *testing.T) {
	spec := ComponentSpec{Namespace: client.DefaultNamespace, Name: "my_app.v2"}
	if err := validateNaming(spec); !errors.Is(err, ErrInvalidName) {
		t.Errorf("validateNaming(%q) = %v, want ErrInvalidName", spec.Name, err)
	}
}

func TestNamingNamespaced(t *testing.T) {
	spec := ComponentSpec{Namespace: "acme-prod", Name: "todo-api"}

	if got := ContainerName(spec); got != "acme-prod_todo-api" {
		t.Errorf("ContainerName = %q", got)
	}
	if got := displayName(spec); got != "acme-prod/todo-api" {
		t.Errorf("displayName = %q", got)
	}

	// The short alias comes first and is the address a sibling uses — that is
	// what lets the same repo deploy into any namespace without editing URLs.
	aliases := networkAliases(spec)
	if len(aliases) != 2 || aliases[0] != "todo-api" || aliases[1] != "acme-prod_todo-api" {
		t.Fatalf("networkAliases = %v, want [short, qualified]", aliases)
	}
}

func TestValidateNaming(t *testing.T) {
	valid := []ComponentSpec{
		{Namespace: "acme", Name: "api"},
		{Namespace: "acme-prod-2", Name: "todo-api"},
		{Namespace: "a", Name: "b"},
	}
	for _, spec := range valid {
		if err := validateNaming(spec); err != nil {
			t.Errorf("validateNaming(%+v) = %v, want nil", spec, err)
		}
	}

	invalid := map[string]ComponentSpec{
		"empty name":           {Namespace: "acme"},
		"uppercase namespace":  {Namespace: "Acme", Name: "api"},
		"underscore namespace": {Namespace: "acme_prod", Name: "api"},
		"underscore name":      {Namespace: "acme", Name: "my_api"},
		"dot in name":          {Namespace: "acme", Name: "api.v2"},
		"leading dash":         {Namespace: "acme", Name: "-api"},
		"trailing dash":        {Namespace: "acme", Name: "api-"},
		"namespace with slash": {Namespace: "acme/prod", Name: "api"},
		"name too long":        {Namespace: "acme", Name: string(make([]byte, 64))},
	}
	for name, spec := range invalid {
		t.Run(name, func(t *testing.T) {
			err := validateNaming(spec)
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !errors.Is(err, ErrInvalidName) {
				t.Fatalf("error %v does not wrap ErrInvalidName", err)
			}
		})
	}
}

// The separator must make the qualified name unambiguous: forbidding "_" in
// both halves is what stops namespace "a" + name "b-c" from colliding with
// namespace "a-b" + name "c".
func TestContainerNameCannotCollide(t *testing.T) {
	one := ComponentSpec{Namespace: "a", Name: "b-c"}
	two := ComponentSpec{Namespace: "a-b", Name: "c"}

	if err := validateNaming(one); err != nil {
		t.Fatal(err)
	}
	if err := validateNaming(two); err != nil {
		t.Fatal(err)
	}
	if ContainerName(one) == ContainerName(two) {
		t.Fatalf("both specs produced the container name %q", ContainerName(one))
	}
}

func TestSameIdentity(t *testing.T) {
	a := ComponentSpec{Namespace: "one", Name: "api"}
	b := ComponentSpec{Namespace: "two", Name: "api"}
	c := ComponentSpec{Namespace: "one", Name: "api"}

	if sameIdentity(a, b) {
		t.Error("same name in different namespaces must not collide")
	}
	if !sameIdentity(a, c) {
		t.Error("same namespace and name must collide")
	}
	// A namespaced component and an unnamespaced one with the same name are
	// different components.
	if sameIdentity(a, ComponentSpec{Name: "api"}) {
		t.Error("namespaced and unnamespaced components must not collide")
	}
}

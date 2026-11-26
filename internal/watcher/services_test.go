package watcher

import (
	"strings"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

// `type: postgres` alone must be a complete declaration — that is the whole
// promise of the defaults table.
func TestResolveServiceDefaults(t *testing.T) {
	w := testWatcher(t, "open")
	target := &Target{Name: "todo", ComponentName: "todo-api"}
	state := map[string]serviceState{}

	svc, changed, err := w.resolveService(target, "", "db", manifestService{Type: "postgres"}, state, newInterpolationScope(w.newSecretResolver("default")))
	if err != nil {
		t.Fatalf("resolveService: %v", err)
	}
	if !changed {
		t.Error("a first-time password should mark the state dirty")
	}
	if svc.component != "todo-api-db" {
		t.Errorf("component = %q", svc.component)
	}
	if svc.image != "postgres:16-alpine" || svc.port != 5432 {
		t.Errorf("image/port = %q/%d", svc.image, svc.port)
	}
	// Identity defaults to the app: one app, one database named after it.
	if svc.user != "todo_api" || svc.database != "todo_api" {
		t.Errorf("user/database = %q/%q", svc.user, svc.database)
	}
	if svc.password == "" {
		t.Error("no password generated")
	}
	if svc.env["POSTGRES_PASSWORD"] != svc.password || svc.env["POSTGRES_DB"] != svc.database {
		t.Errorf("bootstrap env not substituted: %v", svc.env)
	}
	// A stateful service always gets a volume, or a redeploy destroys data.
	if len(svc.mounts) != 1 || svc.mounts[0].ContainerPath != "/var/lib/postgresql/data" {
		t.Fatalf("data volume missing: %+v", svc.mounts)
	}
	if svc.health == nil || svc.health.Type != "exec" {
		t.Fatalf("health probe missing: %+v", svc.health)
	}
	if strings.Join(svc.health.ExecCommand, " ") != "pg_isready -U todo_api" {
		t.Errorf("health command = %v", svc.health.ExecCommand)
	}
	// Private by default: reaching it from the host is opt-in.
	if len(svc.ports) != 0 {
		t.Errorf("service published ports without being asked: %+v", svc.ports)
	}
	if !strings.HasPrefix(svc.url(), "postgres://todo_api:") ||
		!strings.HasSuffix(svc.url(), "@todo-api-db:5432/todo_api") {
		t.Errorf("url = %q", svc.url())
	}
}

// The single most damaging failure mode this feature can have: a password
// that changes on redeploy leaves the app locked out of its own data, with
// "authentication failed" as the only clue.
func TestResolveServicePasswordIsStable(t *testing.T) {
	w := testWatcher(t, "open")
	target := &Target{Name: "demo", ComponentName: "demo"}
	state := map[string]serviceState{}

	first, changed, err := w.resolveService(target, "", "db", manifestService{Type: "postgres"}, state, newInterpolationScope(w.newSecretResolver("default")))
	if err != nil {
		t.Fatal(err)
	}
	if !changed || state["db"].Password != first.password {
		t.Fatalf("generated password was not recorded in the state: %+v", state)
	}

	second, changed, err := w.resolveService(target, "", "db", manifestService{Type: "postgres"}, state, newInterpolationScope(w.newSecretResolver("default")))
	if err != nil {
		t.Fatal(err)
	}
	if second.password != first.password {
		t.Errorf("password changed between deploys: %q -> %q", first.password, second.password)
	}
	if changed {
		t.Error("state marked dirty on a redeploy with an existing password")
	}
}

func TestResolveServiceExplicitPasswordWins(t *testing.T) {
	w := testWatcher(t, "open")
	t.Setenv("QOOQ_TEST_DB_PW", "operator-chosen")
	state := map[string]serviceState{"db": {Password: "generated"}}

	svc, changed, err := w.resolveService(&Target{Name: "demo", ComponentName: "demo"}, "",
		"db", manifestService{Type: "postgres", Password: "${env.QOOQ_TEST_DB_PW}"}, state, newInterpolationScope(w.newSecretResolver("default")))
	if err != nil {
		t.Fatal(err)
	}
	if svc.password != "operator-chosen" {
		t.Errorf("password = %q, want the manifest's", svc.password)
	}
	if changed {
		t.Error("an explicit password should not rewrite the stored state")
	}
}

func TestResolveServiceOverrides(t *testing.T) {
	w := testWatcher(t, "open")
	decl := manifestService{
		Type:     "postgres",
		Version:  "15",
		User:     "custom_user",
		Database: "custom_db",
		Password: "plaintext",
		Ports:    []string{"15432:5432"},
		Storage:  manifestServiceStorage{Volume: "pgdata", Path: "/custom/data"},
		Env:      map[string]string{"EXTRA": "value"},
	}
	svc, _, err := w.resolveService(&Target{Name: "demo", ComponentName: "demo"}, "", "db", decl, map[string]serviceState{}, newInterpolationScope(w.newSecretResolver("default")))
	if err != nil {
		t.Fatal(err)
	}
	if svc.image != "postgres:15-alpine" {
		t.Errorf("version override ignored: %q", svc.image)
	}
	if svc.user != "custom_user" || svc.database != "custom_db" || svc.password != "plaintext" {
		t.Errorf("identity overrides ignored: %+v", svc)
	}
	if len(svc.ports) != 1 || svc.ports[0].HostPort != 15432 {
		t.Errorf("published port not honored: %+v", svc.ports)
	}
	if len(svc.mounts) != 1 || svc.mounts[0].ContainerPath != "/custom/data" ||
		!strings.HasSuffix(svc.mounts[0].HostPath, "/volumes/pgdata") {
		t.Errorf("storage overrides ignored: %+v", svc.mounts)
	}
	if svc.env["EXTRA"] != "value" || svc.env["POSTGRES_USER"] != "custom_user" {
		t.Errorf("env not merged over defaults: %v", svc.env)
	}
}

func TestResolveServiceRejects(t *testing.T) {
	w := testWatcher(t, "open")
	target := &Target{Name: "demo", ComponentName: "demo"}

	cases := map[string]manifestService{
		"unknown type":          {Type: "cassandra"},
		"custom without image":  {Type: "custom"},
		"replicas above one":    {Type: "postgres", Replicas: 3},
		"unsupported mode":      {Type: "postgres", Mode: "cluster"},
		"custom without a port": {Type: "custom", Image: "acme/thing:1"},
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := w.resolveService(target, "", "svc", decl, map[string]serviceState{}, newInterpolationScope(w.newSecretResolver("default"))); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

// Reserved cluster fields must fail loudly: quietly running one instance
// where three were asked for is worse than refusing.
func TestResolveServiceClusterFieldsAreReserved(t *testing.T) {
	w := testWatcher(t, "open")
	_, _, err := w.resolveService(&Target{Name: "demo", ComponentName: "demo"}, "",
		"db", manifestService{Type: "postgres", Replicas: 3}, map[string]serviceState{}, newInterpolationScope(w.newSecretResolver("default")))
	if err == nil || !strings.Contains(err.Error(), "only 1 is supported") {
		t.Fatalf("error = %v, want a clear not-yet-supported message", err)
	}
}

func TestEnvPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"db":         "DB",
		"main-cache": "MAIN_CACHE",
		"queue":      "QUEUE",
	} {
		if got := envPrefix(in); got != want {
			t.Errorf("envPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// Encrypted or not, what goes in must come out — a password that cannot be
// read back is the same failure as one that changed.
func TestServiceStateRoundTrip(t *testing.T) {
	for _, key := range []string{"", "a-secret-passphrase"} {
		w := testWatcher(t, "open")
		w.secretKey = key
		target := &Target{Name: "demo", ComponentName: "demo"}

		data, err := marshalServiceState(map[string]serviceState{"db": {Password: "p@ss"}}, key)
		if err != nil {
			t.Fatalf("key=%q: %v", key, err)
		}
		target.ServiceState = data

		got, err := w.loadServiceState(target)
		if err != nil {
			t.Fatalf("key=%q: %v", key, err)
		}
		if got["db"].Password != "p@ss" {
			t.Errorf("key=%q: round trip lost the password: %+v", key, got)
		}
		if key != "" && strings.Contains(data, "p@ss") {
			t.Errorf("key=%q: password stored in plaintext: %s", key, data)
		}
	}
}

// An app redeploy must not bounce a database whose spec did not change.
func TestServiceUnchanged(t *testing.T) {
	req := client.DeployRequest{
		Name:          "demo-db",
		Image:         "postgres:16-alpine",
		Network:       "qooq-demo",
		RestartPolicy: "always",
		Env:           map[string]string{"POSTGRES_USER": "demo"},
		Mounts:        []client.MountSpec{{HostPath: "/h/data", ContainerPath: "/var/lib/postgresql/data"}},
		HealthCheck:   &client.HealthCheckSpec{Type: "exec", ExecCommand: []string{"pg_isready", "-U", "demo"}},
		Ingress:       &client.IngressSpec{Off: true},
	}
	running := client.ComponentResponse{
		Ingress:       &client.IngressSpec{Off: true},
		Name:          "demo-db",
		Image:         "postgres:16-alpine",
		Network:       "qooq-demo",
		RestartPolicy: "always",
		Env:           map[string]string{"POSTGRES_USER": "demo"},
		Mounts:        []client.MountSpec{{HostPath: "/h/data", ContainerPath: "/var/lib/postgresql/data"}},
		// Server-filled timings must not read as a difference.
		HealthCheck: &client.HealthCheckSpec{
			Type: "exec", ExecCommand: []string{"pg_isready", "-U", "demo"},
			IntervalSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3, InitialDelaySeconds: 5,
		},
	}
	if !serviceUnchanged(&running, req) {
		t.Error("identical spec reported as changed: the database would be restarted on every app deploy")
	}

	changes := map[string]func(*client.ComponentResponse){
		"image":    func(c *client.ComponentResponse) { c.Image = "postgres:15-alpine" },
		"network":  func(c *client.ComponentResponse) { c.Network = "other" },
		"env":      func(c *client.ComponentResponse) { c.Env = map[string]string{"POSTGRES_USER": "other"} },
		"password": func(c *client.ComponentResponse) { c.Env["POSTGRES_PASSWORD"] = "new" },
		"mount":    func(c *client.ComponentResponse) { c.Mounts[0].HostPath = "/elsewhere" },
		"health":   func(c *client.ComponentResponse) { c.HealthCheck.ExecCommand = []string{"true"} },
		// A service from before Ingress.Off: redeployed once to get it.
		"ingress": func(c *client.ComponentResponse) { c.Ingress = nil },
	}
	for name, mutate := range changes {
		t.Run(name, func(t *testing.T) {
			modified := running
			modified.Env = map[string]string{"POSTGRES_USER": "demo"}
			modified.Mounts = []client.MountSpec{{HostPath: "/h/data", ContainerPath: "/var/lib/postgresql/data"}}
			hc := *running.HealthCheck
			modified.HealthCheck = &hc
			mutate(&modified)
			if serviceUnchanged(&modified, req) {
				t.Errorf("a changed %s was reported as unchanged", name)
			}
		})
	}
}

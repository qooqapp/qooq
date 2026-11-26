package watcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

func resolveType(t *testing.T, typ string) resolvedService {
	t.Helper()
	w := testWatcher(t, "open")
	svc, _, err := w.resolveService(&Target{Name: "app", ComponentName: "app"}, "", "svc",
		manifestService{Type: typ}, map[string]serviceState{}, newInterpolationScope(w.newSecretResolver("default")))
	if err != nil {
		t.Fatalf("resolveService(%s): %v", typ, err)
	}
	return svc
}

// The URL the app is handed has to be one the server accepts. redis used to
// get a user and a database it has no notion of, with a password the server
// was never given (WRONGPASS); mongo authenticated against the app's database
// while its user lives in admin ("Authentication failed").
func TestServiceURLsAreOnesTheServerAccepts(t *testing.T) {
	redis := resolveType(t, "redis")
	if redis.user != "default" || redis.database != "" {
		t.Errorf("redis user/database = %q/%q, want its built-in user and no database", redis.user, redis.database)
	}
	if want := "redis://default:" + redis.password + "@app-svc:6379"; redis.url() != want {
		t.Errorf("redis url = %q, want %q", redis.url(), want)
	}
	if redis.env["REDISCLI_AUTH"] != redis.password ||
		!strings.Contains(strings.Join(redis.command, " "), `--requirepass "$REDISCLI_AUTH"`) {
		t.Errorf("redis server is not given the password: env=%v command=%v", redis.env, redis.command)
	}

	mongo := resolveType(t, "mongo")
	if !strings.HasSuffix(mongo.url(), "@app-svc:27017/app?authSource=admin") {
		t.Errorf("mongo url = %q, want authSource=admin", mongo.url())
	}
}

// A probe command is printed in qooqd's log and in `component get`, so a
// built-in one must not carry the password.
func TestNoBuiltInProbeCarriesThePassword(t *testing.T) {
	for typ := range serviceDefaults {
		if typ == "custom" {
			continue
		}
		svc := resolveType(t, typ)
		if svc.health == nil {
			t.Fatalf("%s: no probe", typ)
		}
		if cmd := strings.Join(svc.health.ExecCommand, " "); strings.Contains(cmd, svc.password) {
			t.Errorf("%s probe carries the password: %s", typ, cmd)
		}
	}
	// And a slow first start is not reported as a probe that cannot work.
	if f := resolveType(t, "mysql").health.FailureThreshold; f < 10 {
		t.Errorf("mysql failure threshold = %d; first start takes 20-30s", f)
	}
}

// The generated password reaches the row before the service is created. A
// deploy that failed after the database had initialized its data with it
// used to lose it, and every later build was refused.
func TestAServicePasswordSurvivesAFailedDeploy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error"}`))
	}))
	t.Cleanup(srv.Close)

	w := testWatcherWithDB(t)
	w.client = client.New(srv.URL)
	tg := &Target{Name: "app", RepoURL: "file:///x", ComponentName: "app"}
	if err := w.db.Create(tg).Error; err != nil {
		t.Fatal(err)
	}
	mf := &manifest{Services: map[string]manifestService{"db": {Type: "mysql"}}}
	_, _, err := w.applyServicesTopology(context.Background(), tg, mf, "", "", newInterpolationScope(w.newSecretResolver("default")))
	if err == nil {
		t.Fatal("deploy against a failing daemon succeeded")
	}

	var row Target
	if err := w.db.First(&row, tg.ID).Error; err != nil {
		t.Fatal(err)
	}
	state, err := w.loadServiceState(&row)
	if err != nil {
		t.Fatal(err)
	}
	if state["db"].Password == "" {
		t.Fatal("the password the database may already have been created with was not kept")
	}
}

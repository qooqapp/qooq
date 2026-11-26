package watcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
)

// Starting a project's database goes through qooqd like the app does, so a
// runtime that does not answer (503) is not the repository's fault there
// either. Missing, a docker restart counted against every project with a
// service (E1). Any other failure still counts.
func TestAServiceUpsertTheRuntimeRefusedIsNotTheRepositorysFault(t *testing.T) {
	for _, tc := range []struct {
		code    int
		counted bool
	}{
		{http.StatusServiceUnavailable, false},
		{http.StatusInternalServerError, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.code)
			_, _ = w.Write([]byte(`{"error":"no"}`))
		}))
		w := &Watcher{client: client.New(srv.URL), log: zap.NewNop()}
		err := w.deployService(context.Background(), &Target{Name: "web"},
			resolvedService{name: "db", component: "web-db", image: "postgres:16-alpine"}, "acme--prod", "qooq-acme--prod-web", "")
		srv.Close()
		if err == nil {
			t.Fatalf("%d: no error", tc.code)
		}
		if got := !isNotTheRepository(err); got != tc.counted {
			t.Errorf("%d: counted = %v, want %v (%v)", tc.code, got, tc.counted, err)
		}
	}
}

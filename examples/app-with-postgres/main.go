// app-with-postgres shows what an app receives when its qooq.yaml declares a
// service: the connection details arrive as environment variables, already
// pointing at a database that is up and accepting connections by the time
// this process starts.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	// Injected by qooq from the services: block. Nothing in this repo
	// configures them, and nothing had to be told where the database lives.
	fields := []string{"DB_HOST", "DB_PORT", "DB_USER", "DB_DATABASE", "DATABASE_URL", "MYAPP_DB_DSN"}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "connection details qooq injected:")
		for _, key := range fields {
			fmt.Fprintf(w, "  %s=%s\n", key, redact(os.Getenv(key)))
		}
	})
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("DB_HOST") == "" {
			http.Error(w, "no database was injected", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	log.Printf("db is at %s:%s", os.Getenv("DB_HOST"), os.Getenv("DB_PORT"))
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// redact hides the password inside a URL so the page can be shown to anyone.
func redact(v string) string {
	at := strings.LastIndex(v, "@")
	colon := strings.Index(v, "://")
	if at < 0 || colon < 0 || at < colon {
		return v
	}
	userinfo := v[colon+3 : at]
	if user, _, ok := strings.Cut(userinfo, ":"); ok {
		return v[:colon+3] + user + ":***" + v[at:]
	}
	return v
}

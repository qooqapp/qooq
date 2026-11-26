// config-file-app reads its settings from ./config.toml at startup and
// writes to a data directory — the shape of most real services, and the
// reason qooq.yaml has run.files (config that is versioned with the code but
// changeable without a rebuild) and run.volumes (data that must outlive a
// redeploy).
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// config is the handful of settings this example reads. Parsing is
// hand-rolled (key = "value" lines only) to keep the example dependency-free;
// a real app would use viper, koanf, or encoding/toml.
type config struct {
	greeting string
	dataDir  string
	port     string
}

func loadConfig(path string) (config, error) {
	// Deliberately relative: this is what makes the mount's target path
	// matter. The container's WORKDIR has to be where the file lands.
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	cfg := config{greeting: "hello", dataDir: "/app/data", port: "8080"}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.TrimSpace(key) {
		case "greeting":
			cfg.greeting = value
		case "data_dir":
			cfg.dataDir = value
		case "port":
			cfg.port = value
		}
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig("config.toml")
	if err != nil {
		log.Fatalf("read config.toml from %s: %v (is run.files mounting it?)", mustGetwd(), err)
	}
	log.Printf("config loaded: greeting=%q data_dir=%q port=%s", cfg.greeting, cfg.dataDir, cfg.port)

	if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
		log.Fatalf("create data dir %s: %v", cfg.dataDir, err)
	}

	// Append one line per start. Redeploy a few times and the count keeps
	// climbing — that is the volume doing its job.
	starts := filepath.Join(cfg.dataDir, "starts.log")
	f, err := os.OpenFile(starts, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("open %s: %v (is run.volumes mounting a writable dir?)", starts, err)
	}
	fmt.Fprintf(f, "started at %s\n", time.Now().Format(time.RFC3339))
	f.Close()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		history, _ := os.ReadFile(starts)
		fmt.Fprintf(w, "%s\n\nthis app has started %d time(s); the count survives redeploys\n",
			cfg.greeting, strings.Count(string(history), "\n"))
	})
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("listening on :%s", cfg.port)
	log.Fatal(http.ListenAndServe(":"+cfg.port, nil))
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return wd
}

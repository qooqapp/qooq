// Command qooq-watcher is the M11 skeleton for the GitOps watcher service
// (Phase 2, M11-M13 per the design plan). This milestone only wires up the
// fx/weedbox/common-modules plumbing (config, logger, HTTP server,
// healthcheck, sqlite, daemon) plus the internal/watcher domain module.
// Targets CRUD, git polling and the build/upsert pipeline land in M12/M13.
package main

import (
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"github.com/weedbox/common-modules/configs"
	"github.com/weedbox/common-modules/daemon"
	"github.com/weedbox/common-modules/healthcheck_apis"
	"github.com/weedbox/common-modules/http_server"
	"github.com/weedbox/common-modules/logger"
	"go.uber.org/fx"

	"github.com/qooqapp/qooq/internal/version"
	"github.com/qooqapp/qooq/internal/watcher"
)

// Defaults tuned for a single-VM sidecar service: bind localhost only
// (qooqd itself only binds :7777 on localhost too, see cmd/qooqd) and keep
// the sqlite file out of the shared common-modules default (./data.db).
const (
	defaultHTTPHost = "127.0.0.1"
	defaultHTTPPort = 7778
	defaultDBPath   = "watcher.db"
)

func main() {
	// Checked by hand rather than with the flag package: this binary is
	// configured entirely through viper (env vars and config.yaml), and
	// introducing flag parsing just for this would give it two competing
	// notions of where configuration comes from.
	for _, arg := range os.Args[1:] {
		if arg == "-version" || arg == "--version" {
			fmt.Println("qooq-watcher", version.String())
			return
		}
	}

	// Default gin to release mode (quiet, no debug warnings) for a production
	// sidecar, but respect an explicit GIN_MODE if the operator set one.
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	config := configs.NewConfig("QOOQ_WATCHER")

	// Which store the watcher uses is decided before the graph is built,
	// because it decides which module goes into the graph. Default is sqlite:
	// the standalone line must keep needing no database (db_module.go).
	dbModule, dbScope, err := databaseModule(dbDriver())
	if err != nil {
		panic(err)
	}

	app := fx.New(
		fx.Supply(config),
		logger.Module(),
		http_server.Module("http"),
		healthcheck_apis.Module("healthcheck"),
		dbModule,
		daemon.Module("daemon"),
		watcher.Module("watcher"),
	)

	if err := app.Err(); err != nil {
		panic(err)
	}

	// Each module's Provide/Invoke constructor already ran during fx.New()
	// above and stamped viper's default map with its own defaults
	// (http_server: 0.0.0.0:80, sqlite_connector: ./data.db). viper.SetDefault
	// unconditionally overwrites the previous default (last call wins, see
	// spf13/viper's Viper.SetDefault), so calling it here — after fx.New()
	// but before app.Run(), which is what actually triggers the OnStart
	// hooks that read these keys — reliably wins the race without forking
	// common-modules. Env vars (e.g. QOOQ_WATCHER_HTTP_PORT) still take
	// priority over any default, since viper resolves env before defaults.
	viper.SetDefault("http.host", defaultHTTPHost)
	viper.SetDefault("http.port", defaultHTTPPort)
	// Only meaningful for the sqlite driver; postgres has no path. Guarded so
	// a postgres deployment does not get a stray sqlite.path default that
	// reads as "it is also opening a file somewhere".
	if dbScope == "sqlite" {
		viper.SetDefault("sqlite.path", defaultDBPath)
	}

	app.Run()
}

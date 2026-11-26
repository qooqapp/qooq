package main

import (
	"fmt"

	"github.com/spf13/viper"
	"github.com/weedbox/common-modules/postgres_connector"
	"go.uber.org/fx"
)

// Which database the watcher stores its targets, stacks and secrets in is the
// one difference between the two product lines that reaches this far down.
//
//   - standalone: sqlite, a file next to the rest of ~/.qooq. One process, no
//     database to install — that is the whole offline story and the default
//     must never stop being it.
//   - multi-watcher: postgres, shared by every watcher, because a project's
//     truth cannot live in a file on whichever machine happened to create it.
//
// Both are gorm, and the models carry no driver-specific column types (plain
// not null/default/index), so this is a connector swap rather than a second
// implementation. The one place the drivers genuinely differ is claiming a
// target, and that is handled in internal/watcher/claim.go with a lease that
// works on both.
const (
	driverSQLite   = "sqlite"
	driverPostgres = "postgres"

	// configDBDriver is read before fx.New() — the choice decides which module
	// goes into the graph, so it cannot come from a module's own OnStart.
	// viper resolves QOOQ_WATCHER_DATABASE_DRIVER for it like any other key.
	configDBDriver = "database.driver"
)

// databaseModule returns the connector for the configured driver, and the
// scope it reads its settings from.
//
// An unknown driver is a startup error rather than a fallback to sqlite: a
// typo in QOOQ_WATCHER_DATABASE_DRIVER would otherwise bring the watcher up
// happily against an empty local file while the operator believes it is
// talking to the shared postgres, and every project would look deleted.
func databaseModule(driver string) (fx.Option, string, error) {
	switch driver {
	case "", driverSQLite:
		// Scope stays "sqlite" so QOOQ_WATCHER_SQLITE_PATH and every existing
		// config.yaml keep working untouched.
		return sqliteModule("sqlite"), "sqlite", nil
	case driverPostgres:
		// postgres_connector reads <scope>.host/.port/.user/.password/.dbname
		// plus pool settings, and already sets gorm's TranslateError — which
		// store.go depends on to turn a UNIQUE violation into
		// gorm.ErrDuplicatedKey (that is how a duplicate target name becomes a
		// 409 instead of a 500).
		return postgres_connector.Module("postgres"), "postgres", nil
	default:
		return nil, "", fmt.Errorf("unknown %s %q (known: %s, %s)", configDBDriver, driver, driverSQLite, driverPostgres)
	}
}

// dbDriver reports the configured driver. Called before fx.New(), so it reads
// viper directly: configs.NewConfig already bound the env prefix and loaded
// any config file at package init, long before this.
func dbDriver() string {
	return viper.GetString(configDBDriver)
}

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"github.com/spf13/viper"
	"github.com/weedbox/common-modules/database"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
	gorm_logger "gorm.io/gorm/logger"
)

// sqliteModule is a pure-Go replacement for weedbox's sqlite_connector
// (mattn/go-sqlite3, which needs CGO). It keeps only what the watcher
// actually uses: a single pool with WAL + busy_timeout, and TranslateError
// (store.go relies on it to map UNIQUE violations to gorm.ErrDuplicatedKey —
// glebarez/sqlite implements gorm's ErrorTranslator).
//
// Config stays compatible with the old module: `sqlite.path`
// (env QOOQ_WATCHER_SQLITE_PATH). The default is stamped in the constructor
// during fx.New(), same build-phase semantics as the weedbox modules, so the
// post-fx.New() override in main.go keeps working unchanged.
func sqliteModule(scope string) fx.Option {
	return database.Module(scope, func(p sqliteParams) database.DatabaseConnector {
		c := &sqliteConnector{logger: p.Logger.Named(scope), scope: scope}
		viper.SetDefault(scope+".path", "./data.db")
		p.Lifecycle.Append(fx.Hook{OnStart: c.onStart, OnStop: c.onStop})
		return c
	})
}

type sqliteParams struct {
	fx.In
	Lifecycle fx.Lifecycle
	Logger    *zap.Logger
}

type sqliteConnector struct {
	logger *zap.Logger
	scope  string
	db     *gorm.DB
}

func (c *sqliteConnector) onStart(ctx context.Context) error {
	path := viper.GetString(c.scope + ".path")
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create database directory %s: %w", dir, err)
		}
	}
	c.logger.Info("Starting pure-Go sqlite connector", zap.String("path", path))

	// modernc's driver takes pragmas as repeated _pragma=name(value) params —
	// not mattn's _busy_timeout=/_journal_mode= form.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:         gorm_logger.Default.LogMode(gorm_logger.Error),
		TranslateError: true,
	})
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	// Sweep + API + backup are the only concurrent users; a small pool with
	// busy_timeout is plenty for a single-VM sidecar.
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(1)
	c.db = db
	return nil
}

func (c *sqliteConnector) onStop(ctx context.Context) error {
	c.logger.Info("Stopped pure-Go sqlite connector")
	sqlDB, err := c.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (c *sqliteConnector) GetDB() *gorm.DB {
	return c.db
}

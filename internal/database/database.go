package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

const SchemaVersion = 1

const dirPermissions = 0755

// Postgres connection pool limits. database/sql keeps only two idle
// connections by default, which opens a new Postgres session for almost every
// request under load.
const (
	postgresMaxOpenConns    = 32
	postgresMaxIdleConns    = 32
	postgresConnMaxIdleTime = 5 * time.Minute
	postgresConnMaxLifetime = 30 * time.Minute
)

type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// SQLite DSN parameters. Every connection a pool opens applies them, so a
// connection database/sql reopens gets them too. The reader pool sets
// query_only, which makes a write sent to it fail instead of being run.
const (
	sqliteWriterParams = "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"
	sqliteReaderParams = "?_busy_timeout=5000&_query_only=1"
)

// Size bounds of the SQLite reader pool. At least two, so a slow stats or
// eviction scan does not block request lookups even on one CPU.
const (
	sqliteMinReaders = 2
	sqliteMaxReaders = 8
)

// DB is the proxy database. The embedded pool takes every write. For SQLite
// it holds a single connection, as SQLite allows one writer at a time, and
// reads go to a separate read-only pool so they don't queue behind writes or
// behind each other (WAL mode lets readers run alongside the writer). For
// Postgres and in-memory SQLite both are the same pool.
//
// Get, Select, Query and QueryRow (and their Context forms) use the reader.
// Exec, transactions and everything else use the writer, so a read inside a
// transaction goes through the transaction and sees its own writes.
type DB struct {
	*sqlx.DB
	reader  *sqlx.DB
	dialect Dialect
	path    string
	hits    *hitBatch
}

func (db *DB) Dialect() Dialect {
	return db.dialect
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func Create(path string) (*DB, error) {
	if Exists(path) {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing existing database: %w", err)
		}
	}

	db, err := Open(path)
	if err != nil {
		return nil, err
	}

	if err := db.CreateSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}

	return db, nil
}

func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, dirPermissions); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}
	}

	writer, err := sqlx.Open("sqlite", path+sqliteWriterParams)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// One connection serializes writes, so they never wait on SQLite's
	// busy timeout for each other.
	writer.SetMaxOpenConns(1)

	// Connecting applies journal_mode=WAL before any reader opens the file.
	if err := writer.Ping(); err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("opening database: %w", err)
	}

	db := &DB{DB: writer, reader: writer, dialect: DialectSQLite, path: path}
	if isMemoryPath(path) {
		// Each connection to an in-memory database gets its own empty
		// database, so the reads have to share the writer's connection.
		return db, nil
	}

	reader, err := sqlx.Open("sqlite", path+sqliteReaderParams)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("opening database reader: %w", err)
	}
	n := sqliteReaderConns()
	reader.SetMaxOpenConns(n)
	reader.SetMaxIdleConns(n)
	db.reader = reader

	return db, nil
}

func sqliteReaderConns() int {
	return min(max(runtime.GOMAXPROCS(0), sqliteMinReaders), sqliteMaxReaders)
}

func isMemoryPath(path string) bool {
	return path == "" || strings.HasPrefix(path, ":memory:") || strings.Contains(path, "mode=memory")
}

func OpenOrCreate(path string) (*DB, error) {
	if Exists(path) {
		return Open(path)
	}
	return Create(path)
}

func OpenPostgres(url string) (*DB, error) {
	sqlDB, err := sqlx.Open("postgres", url)
	if err != nil {
		return nil, fmt.Errorf("opening postgres database: %w", err)
	}

	sqlDB.SetMaxOpenConns(postgresMaxOpenConns)
	sqlDB.SetMaxIdleConns(postgresMaxIdleConns)
	sqlDB.SetConnMaxIdleTime(postgresConnMaxIdleTime)
	sqlDB.SetConnMaxLifetime(postgresConnMaxLifetime)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	return &DB{DB: sqlDB, reader: sqlDB, dialect: DialectPostgres}, nil
}

func OpenPostgresOrCreate(url string) (*DB, error) {
	db, err := OpenPostgres(url)
	if err != nil {
		return nil, err
	}

	// Instances started together on an empty database would otherwise
	// create the schema at the same time and fail on each other's tables.
	err = db.WithLock(context.Background(), schemaLock, func() error {
		var exists bool
		if err := db.Get(&exists, "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'schema_info')"); err != nil {
			return fmt.Errorf("checking schema: %w", err)
		}
		if exists {
			return nil
		}
		if err := db.CreateSchema(); err != nil {
			return fmt.Errorf("creating schema: %w", err)
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func (db *DB) OptimizeForBulkWrites() error {
	if db.dialect == DialectPostgres {
		return nil
	}
	_, err := db.Exec(`
		PRAGMA synchronous = OFF;
		PRAGMA journal_mode = WAL;
		PRAGMA cache_size = -64000;
	`)
	return err
}

func (db *DB) OptimizeForReads() error {
	if db.dialect == DialectPostgres {
		return nil
	}
	_, err := db.Exec(`
		PRAGMA synchronous = NORMAL;
		PRAGMA journal_mode = WAL;
	`)
	return err
}

func (db *DB) Path() string {
	return db.path
}

func (db *DB) Rebind(query string) string {
	return db.DB.Rebind(query)
}

// Get runs a read on the reader pool.
func (db *DB) Get(dest any, query string, args ...any) error {
	return db.reader.Get(dest, query, args...)
}

// GetContext runs a read on the reader pool.
func (db *DB) GetContext(ctx context.Context, dest any, query string, args ...any) error {
	return db.reader.GetContext(ctx, dest, query, args...)
}

// Select runs a read on the reader pool.
func (db *DB) Select(dest any, query string, args ...any) error {
	return db.reader.Select(dest, query, args...)
}

// SelectContext runs a read on the reader pool.
func (db *DB) SelectContext(ctx context.Context, dest any, query string, args ...any) error {
	return db.reader.SelectContext(ctx, dest, query, args...)
}

// Query runs a read on the reader pool.
func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.reader.Query(query, args...)
}

// QueryContext runs a read on the reader pool.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.reader.QueryContext(ctx, query, args...)
}

// QueryRow runs a read on the reader pool.
func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	return db.reader.QueryRow(query, args...)
}

// QueryRowContext runs a read on the reader pool.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.reader.QueryRowContext(ctx, query, args...)
}

// closePools closes the reader pool, if separate, then the writer. The
// writer goes last so that, as the last connection, it checkpoints the WAL.
func (db *DB) closePools() error {
	var readerErr error
	if db.reader != nil && db.reader != db.DB {
		readerErr = db.reader.Close()
	}
	if err := db.DB.Close(); err != nil {
		return err
	}
	return readerErr
}

package controlplane

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// Storage adapters. SQLite is the default and runs exactly as before; a
// `postgres://` (or `postgresql://`) database URL switches the same store
// onto Postgres. The adapter is a rewriting database/sql driver: every
// statement goes through postgresSQL first, which rewrites SQLite
// placeholders and the handful of SQLite-specific expressions (the JSON
// functions) that Postgres spells differently. Everything else — upserts,
// partial indexes, CHECK constraints — is already common SQL.

const (
	dialectSQLite   = "sqlite"
	dialectPostgres = "postgres"
)

// postgresDialect reports whether a configured database is a Postgres URL.
func postgresDialect(database string) bool {
	database = strings.TrimSpace(database)
	return strings.HasPrefix(database, "postgres://") || strings.HasPrefix(database, "postgresql://")
}

// openStoreDialect opens the configured database and runs schema
// initialization. A filesystem path opens SQLite (the default); a
// postgres:// or postgresql:// URL opens Postgres. Artifact storage
// resolution lives in OpenStore (store.go).
func openStoreDialect(database string) (*Store, error) {
	if postgresDialect(database) {
		return openPostgresStore(database)
	}
	if dir := filepath.Dir(database); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", database)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite serialises writers; one connection avoids SQLITE_BUSY entirely.
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: database, dialect: dialectSQLite, now: time.Now, startedAt: time.Now().UTC(), leaseGrace: defaultLeaseGrace}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// ensureSimpleProtocol forces pgx's simple query protocol. Schema
// initialization sends multi-statement batches (helpers, triggers, the whole
// DDL), which the extended protocol rejects with a syntax error on the
// second statement.
func ensureSimpleProtocol(url string) string {
	if strings.Contains(url, "default_query_exec_mode=") {
		return url
	}
	separator := "?"
	if strings.Contains(url, "?") {
		separator = "&"
	}
	return url + separator + "default_query_exec_mode=simple_protocol"
}

// postgresDriver wraps the pgx stdlib driver, rewriting every statement on
// its way down. Registered once per process.
const postgresDriverName = "machinist-postgres"

var postgresDriverRegistered bool

func registerPostgresDriver() {
	if postgresDriverRegistered {
		return
	}
	sql.Register(postgresDriverName, &postgresRewriteDriver{underlying: stdlib.GetDefaultDriver()})
	postgresDriverRegistered = true
}

type postgresRewriteDriver struct {
	underlying driver.Driver
}

func (d *postgresRewriteDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.underlying.Open(name)
	if err != nil {
		return nil, err
	}
	return &postgresRewriteConn{Conn: conn}, nil
}

func (d *postgresRewriteDriver) OpenConnector(name string) (driver.Connector, error) {
	connectorProvider, ok := d.underlying.(interface {
		OpenConnector(name string) (driver.Connector, error)
	})
	if !ok {
		return nil, fmt.Errorf("underlying driver does not support OpenConnector")
	}
	connector, err := connectorProvider.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return &postgresRewriteConnector{Connector: connector}, nil
}

type postgresRewriteConnector struct {
	driver.Connector
}

func (c *postgresRewriteConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &postgresRewriteConn{Conn: conn}, nil
}

type postgresRewriteConn struct {
	driver.Conn
}

func (c *postgresRewriteConn) prepareQuery(query string) string {
	rewritten := postgresSQL(query)
	if os.Getenv("MACHINIST_DEBUG_REWRITE") != "" && rewritten != query {
		fmt.Fprintf(os.Stderr, "REWRITE:\nIN:  %s\nOUT: %s\n", query, rewritten)
	}
	return rewritten
}

func (c *postgresRewriteConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, c.prepareQuery(query), args)
	}
	return nil, driver.ErrSkip
}

func (c *postgresRewriteConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		return execer.ExecContext(ctx, c.prepareQuery(query), args)
	}
	return nil, driver.ErrSkip
}

// Prepare keeps the rewrite for clients that prepare explicitly; database/sql
// prefers the conn-level Query/Exec contexts above, which also rewrite.
func (c *postgresRewriteConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(c.prepareQuery(query))
}

func openPostgresStore(url string) (*Store, error) {
	registerPostgresDriver()
	url = ensureSimpleProtocol(url)
	db, err := sql.Open(postgresDriverName, url)
	if err != nil {
		return nil, fmt.Errorf("open postgres database: %w", err)
	}
	// The Postgres adapter relies on transactional DDL and statement
	// rewriting; verify the connection before claiming the store works.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect postgres database: %w", err)
	}
	store := &Store{db: db, path: url, dialect: dialectPostgres, now: time.Now, startedAt: time.Now().UTC(), leaseGrace: defaultLeaseGrace}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// placeholderPattern matches ? placeholders outside single-quoted strings, so
// literals like 'state IN (?, ?)' text are never mangled.
var placeholderPattern = regexp.MustCompile(`\?`)

// postgresSQL rewrites one statement for Postgres: numbered placeholders, the
// SQLite JSON functions (mapped onto helper functions the schema installs),
// INSERT OR IGNORE, and rowid (mapped onto the rowseq identity columns the
// schema adds to jobs, runs and job_state_history). Statements that need no
// rewrite pass through unchanged.
func postgresSQL(query string) string {
	if !strings.Contains(query, "?") && !strings.Contains(query, "json_") &&
		!strings.Contains(query, "rowid") {
		return query
	}
	if strings.Contains(strings.ToUpper(query), "ROWID") {
		query = regexp.MustCompile(`\browid\b`).ReplaceAllString(query, "rowseq")
	}
	query = replaceJSONFunctions(query)
	var builder strings.Builder
	inString := false
	number := 0
	for index := range query {
		char := query[index]
		switch {
		case char == '\'':
			inString = !inString
			builder.WriteByte(char)
		case char == '?' && !inString:
			number++
			builder.WriteByte('$')
			builder.WriteString(strconv.Itoa(number))
		default:
			builder.WriteByte(char)
		}
	}
	return builder.String()
}

// replaceJSONFunctions swaps the SQLite JSON functions for the tf_json_*
// helpers the Postgres schema installs. The paths used in this codebase are
// always of the form '$.key'.
func replaceJSONFunctions(query string) string {
	query = regexp.MustCompile(`\bjson_valid\(`).ReplaceAllString(query, "tf_json_valid(")
	query = regexp.MustCompile(`\bjson_extract\(`).ReplaceAllString(query, "tf_json_extract(")
	return query
}

var errSQLiteOnly = errors.New("statement is SQLite-specific and cannot run on Postgres")

// rowseqColumns lists the tables whose insertion order statements depend on;
// the Postgres schema adds a rowseq identity column to each.
var rowseqTables = []string{"jobs", "runs", "job_state_history"}

// schemaExtensions appends the adapter-only columns (rowseq) to the
// translated DDL. GENERATED BY DEFAULT fires on every insert that omits the
// column, reproducing SQLite rowid ordering; requeues keep their original
// position because updates never touch it.
func schemaExtensions() string {
	var builder strings.Builder
	for _, table := range rowseqTables {
		builder.WriteString(fmt.Sprintf("\nALTER TABLE %s ADD COLUMN IF NOT EXISTS rowseq BIGINT GENERATED BY DEFAULT AS IDENTITY;", table))
	}
	return builder.String()
}

// columnExists is the dialect-safe pragma_table_info('t') WHERE name='c'.
func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	if s.dialect == dialectPostgres {
		var present int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2`, table, column).Scan(&present)
		return present > 0, err
	}
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&present)
	return present > 0, err
}

// tableExists is the dialect-safe sqlite_master check.
func (s *Store) tableExists(ctx context.Context, table string) (bool, error) {
	if s.dialect == dialectPostgres {
		var present int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_name=$1`, table).Scan(&present)
		return present > 0, err
	}
	var present int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&present)
	return present > 0, err
}

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The Postgres adapter: the same Store, the same statements, one translation
// layer. Statements flow through the rewriting driver (dialect.go); this file
// holds schema initialization — helpers, triggers, version marker, and the
// DDL transform that turns the shared SQLite schema into Postgres DDL.

// initializePostgres brings a Postgres database to the current schema: the
// shared baseSchema plus the extension constants, run through postgresDDL.
// There is no migration chain on Postgres — a fresh database is created at
// the current version, and CREATE IF NOT EXISTS keeps repeat boots
// idempotent, so the version marker is only written once.
func (s *Store) initializePostgres(ctx context.Context, schemaVersion int) error {
	if _, err := s.db.ExecContext(ctx, postgresHelpers); err != nil {
		return fmt.Errorf("install postgres helpers: %w", err)
	}
	version, err := s.postgresSchemaVersion(ctx)
	if err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	ddl := postgresDDL(baseSchema+workflowSchema+artifactSchema+reviewSchema+jobMetadataSchema+planeSchema+autoMergeSchema+ticketSchema) + postgresTriggers
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("initialize postgres schema: %w", err)
	}
	// Column guards are dialect-safe (they check information_schema) and
	// idempotent; they close the gap between the shared schema and the
	// columns the SQLite migration chain used to add.
	for _, guard := range []func(context.Context) error{
		s.addJobTitle, s.addJobAttemptColumns, s.addJobResolution, s.addRunSkills, s.addRunWIP, s.addTicketProjectColumns,
	} {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	if version < schemaVersion {
		if err := s.setPostgresSchemaVersion(ctx, schemaVersion); err != nil {
			return err
		}
	}
	return s.migrateReviewInstructions(ctx)
}

// postgresSchemaVersion reads the adapter's version marker; an empty
// database reports 0.
func (s *Store) postgresSchemaVersion(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS machinist_schema_version (version BIGINT NOT NULL)`); err != nil {
		return 0, fmt.Errorf("create schema version table: %w", err)
	}
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM machinist_schema_version`).Scan(&version); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return int(version.Int64), nil
}

func (s *Store) setPostgresSchemaVersion(ctx context.Context, version int) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM machinist_schema_version`); err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO machinist_schema_version(version) VALUES($1)`, version); err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}
	return nil
}

// postgresHelpers installs the JSON helpers the rewritten statements call.
// SQLite's json_valid/json_extract tolerate arbitrary text; the helpers keep
// that tolerance (malformed JSON reads as NULL, never an error).
const postgresHelpers = `
CREATE OR REPLACE FUNCTION tf_json_valid(value TEXT) RETURNS BOOLEAN LANGUAGE plpgsql IMMUTABLE AS $body$
BEGIN
	IF value IS NULL THEN RETURN FALSE; END IF;
	PERFORM value::jsonb;
	RETURN TRUE;
EXCEPTION WHEN OTHERS THEN
	RETURN FALSE;
END $body$;

CREATE OR REPLACE FUNCTION tf_json_extract(value TEXT, path TEXT) RETURNS TEXT LANGUAGE plpgsql IMMUTABLE AS $body$
DECLARE
	result TEXT;
BEGIN
	IF value IS NULL OR path IS NULL THEN RETURN NULL; END IF;
	EXECUTE 'SELECT ($1::jsonb ->> $2)::text' INTO result USING value, trim(leading '$.' from path);
	RETURN result;
EXCEPTION WHEN OTHERS THEN
	RETURN NULL;
END $body$;`

// postgresTriggers replaces the two SQLite triggers in baseSchema: Postgres
// expresses the same bookkeeping as one function and two trigger bindings.
const postgresTriggers = `
CREATE OR REPLACE FUNCTION tf_job_state_history() RETURNS trigger LANGUAGE plpgsql AS $body$
BEGIN
	IF TG_OP = 'INSERT' THEN
		INSERT INTO job_state_history(job_id,state,changed_at) VALUES (new.id,new.state,new.created_at);
		RETURN new;
	END IF;
	IF old.state IS DISTINCT FROM new.state THEN
		INSERT INTO job_state_history(job_id,state,changed_at) VALUES (new.id,new.state,new.updated_at);
	END IF;
	RETURN new;
END $body$;

DROP TRIGGER IF EXISTS job_state_history_insert ON jobs;
CREATE TRIGGER job_state_history_insert AFTER INSERT ON jobs
FOR EACH ROW EXECUTE FUNCTION tf_job_state_history();

DROP TRIGGER IF EXISTS job_state_history_update ON jobs;
CREATE TRIGGER job_state_history_update AFTER UPDATE OF state ON jobs
FOR EACH ROW EXECUTE FUNCTION tf_job_state_history();`

// triggerBodyPattern matches a SQLite `CREATE TRIGGER ... BEGIN ... END;`
// block, including its leading newline, so DDL translation can drop it.
var triggerBodyPattern = regexp.MustCompile(`(?s)\n?CREATE TRIGGER IF NOT EXISTS [^` + "`" + `]*?END;`)

// pragmaPattern matches a SQLite PRAGMA statement on its own line.
var pragmaPattern = regexp.MustCompile(`(?m)^PRAGMA[^;]*;\n?`)

// postgresDDL translates the shared SQLite schema to Postgres: 64-bit
// integers everywhere (SQLite INTEGER is 64-bit; UnixNano lease timestamps
// overflow Postgres INTEGER), identity keys where SQLite autoincremented,
// JSON functions remapped, PRAGMAs and SQLite trigger bodies dropped.
func postgresDDL(schema string) string {
	schema = triggerBodyPattern.ReplaceAllString(schema, "")
	schema = pragmaPattern.ReplaceAllString(schema, "")
	schema = strings.ReplaceAll(schema, "INTEGER PRIMARY KEY AUTOINCREMENT", "BIGINT GENERATED BY DEFAULT AS IDENTITY")
	schema = regexp.MustCompile(`\bINTEGER\b`).ReplaceAllString(schema, "BIGINT")
	schema = strings.ReplaceAll(schema, "json_valid(", "tf_json_valid(")
	schema = strings.ReplaceAll(schema, "json_extract(", "tf_json_extract(")
	// rowseq rides inside the CREATE TABLE statements so the DDL's own
	// INSERT ... ORDER BY rowseq statements see it immediately.
	for _, table := range rowseqTables {
		needle := "CREATE TABLE IF NOT EXISTS " + table + " (\n"
		if !strings.Contains(schema, needle) {
			needle = "CREATE TABLE IF NOT EXISTS " + table + "("
		}
		if strings.Contains(schema, needle) {
			schema = strings.Replace(schema, needle, needle+"\n rowseq BIGINT GENERATED BY DEFAULT AS IDENTITY,", 1)
		}
	}
	// Statement-scoped rewrites: INSERT OR IGNORE becomes ON CONFLICT DO
	// NOTHING, valid on both dialects, attached to its own statement.
	statements := strings.Split(schema, ";\n")
	for index, statement := range statements {
		if !strings.Contains(statement, "INSERT OR IGNORE INTO") {
			continue
		}
		statement = strings.Replace(statement, "INSERT OR IGNORE INTO", "INSERT INTO", 1)
		statements[index] = statement + "\nON CONFLICT DO NOTHING"
	}
	// rowseq rides inside the CREATE TABLE statements so the DDL's own
	// INSERT ... ORDER BY rowseq statements see it immediately.
	for _, table := range rowseqTables {
		needle := "CREATE TABLE IF NOT EXISTS " + table + " (\n"
		if !strings.Contains(schema, needle) {
			needle = "CREATE TABLE IF NOT EXISTS " + table + "("
		}
		if strings.Contains(schema, needle) {
			schema = strings.Replace(schema, needle, needle+"\n rowseq BIGINT GENERATED BY DEFAULT AS IDENTITY,", 1)
		}
	}
	return strings.Join(statements, ";\n")
}

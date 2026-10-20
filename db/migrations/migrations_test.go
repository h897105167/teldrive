package migrations

import (
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestRenderedFilesQualifyConfiguredSchema(t *testing.T) {
	t.Parallel()

	rendered, err := renderedFiles("tenant_alpha")
	if err != nil {
		t.Fatalf("render migrations: %v", err)
	}
	entries, err := fs.ReadDir(rendered, ".")
	if err != nil {
		t.Fatalf("read rendered migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected rendered migrations")
	}
	for _, entry := range entries {
		data, err := fs.ReadFile(rendered, entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		text := string(data)
		if strings.Contains(text, schemaTemplateMarker) {
			t.Fatalf("migration %s still contains schema marker", entry.Name())
		}
		if !strings.Contains(text, `"tenant_alpha".`) {
			t.Fatalf("migration %s does not contain qualified schema", entry.Name())
		}
	}
}

func TestRenderedFilesRejectInvalidSchema(t *testing.T) {
	t.Parallel()
	if _, err := renderedFiles(`tenant; DROP SCHEMA public`); err == nil {
		t.Fatal("expected invalid schema to fail")
	}
}

// TestMigrationVersionsAreContiguous guards the migration ledger contract:
// goose compares version numbers only, so a gap or a duplicate silently skips
// files on databases that already ran an earlier numbering of the same set.
func TestMigrationVersionsAreContiguous(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(Files, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	versionPattern := regexp.MustCompile(`^(\d+)_[a-z0-9_]+\.sql$`)
	versions := make([]int, 0, len(entries))
	for _, entry := range entries {
		match := versionPattern.FindStringSubmatch(entry.Name())
		if match == nil {
			t.Fatalf("migration %s does not follow NNNNNN_name.sql", entry.Name())
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("parse version of %s: %v", entry.Name(), err)
		}
		versions = append(versions, version)
	}
	sort.Ints(versions)
	for index, version := range versions {
		want := index + 1
		if version != want {
			t.Fatalf("migration versions must be contiguous from 1: got %v", versions)
		}
	}
}

// TestUserPreferencesCreatedDespiteLegacyLedger pins the repair migration that
// keeps deployments whose ledger already contains version 10 working. Delete
// this test only together with 000014.
func TestUserPreferencesCreatedDespiteLegacyLedger(t *testing.T) {
	t.Parallel()

	const repairMigration = "000014_user_preferences_repair.sql"
	data, err := fs.ReadFile(Files, repairMigration)
	if err != nil {
		t.Fatalf("read %s: %v", repairMigration, err)
	}
	text := string(data)
	if !strings.Contains(text, "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("%s must create the table idempotently", repairMigration)
	}
	if !strings.Contains(text, "user_preferences") {
		t.Fatalf("%s must target user_preferences", repairMigration)
	}
	if !strings.HasPrefix(text, "-- +goose Up") {
		t.Fatalf("%s must start with a goose Up marker", repairMigration)
	}
}

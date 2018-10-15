package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/spanner"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	adminpb "google.golang.org/genproto/googleapis/spanner/admin/database/v1"
)

const (
	envTestProjectId  = "SPANNER_CLI_INTEGRATION_TEST_PROJECT_ID"
	envTestInstanceId = "SPANNER_CLI_INTEGRATION_TEST_INSTANCE_ID"
	envTestDatabaseId = "SPANNER_CLI_INTEGRATION_TEST_DATABASE_ID"
	envTestCredential = "SPANNER_CLI_INTEGRATION_TEST_CREDENTIAL"
)

var (
	skipIntegrateTest bool

	testProjectId  string
	testInstanceId string
	testDatabaseId string
	testCredential string

	tableIdCounter uint32
)

type testTableSchema struct {
	Id     int64 `spanner: "id"`
	Active bool  `spanner: "active"`
}

func TestMain(m *testing.M) {
	initialize()
	os.Exit(m.Run())
}

func initialize() {
	if os.Getenv(envTestProjectId) == "" || os.Getenv(envTestInstanceId) == "" || os.Getenv(envTestDatabaseId) == "" || os.Getenv(envTestCredential) == "" {
		skipIntegrateTest = true
		return
	}

	testProjectId = os.Getenv(envTestProjectId)
	testInstanceId = os.Getenv(envTestInstanceId)
	testDatabaseId = os.Getenv(envTestDatabaseId)
	testCredential = os.Getenv(envTestCredential)
}

func generateUniqueTableId() string {
	count := atomic.AddUint32(&tableIdCounter, 1)
	return fmt.Sprintf("spanner_cli_test_%d_%d", time.Now().UnixNano(), count)
}

func setup(t *testing.T, ctx context.Context, dmls []string) (*Session, string, func()) {
	session, err := NewSession(ctx, testProjectId, testInstanceId, testDatabaseId, spanner.ClientConfig{
		SessionPoolConfig: spanner.SessionPoolConfig{WriteSessions: 0.2},
	}, option.WithCredentialsJSON([]byte(testCredential)))
	if err != nil {
		t.Fatalf("failed to create test session: err=%s", err)
	}

	dbPath := fmt.Sprintf("projects/%s/instances/%s/databases/%s", testProjectId, testInstanceId, testDatabaseId)

	tableId := generateUniqueTableId()
	tableSchema := fmt.Sprintf(`
	CREATE TABLE %s (
	  id INT64 NOT NULL,
	  active BOOL NOT NULL
	) PRIMARY KEY (id)
	`, tableId)

	op, err := session.adminClient.UpdateDatabaseDdl(ctx, &adminpb.UpdateDatabaseDdlRequest{
		Database:   dbPath,
		Statements: []string{tableSchema},
	})
	if err != nil {
		t.Fatalf("failed to create table: err=%s", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("failed to create table: err=%s", err)
	}

	for _, dml := range dmls {
		dml = strings.Replace(dml, "[[TABLE]]", tableId, -1)
		stmt := spanner.NewStatement(dml)
		_, err := session.client.ReadWriteTransaction(session.ctx, func(ctx context.Context, txn *spanner.ReadWriteTransaction) error {
			_, err = txn.Update(ctx, stmt)
			if err != nil {
				t.Fatalf("failed to apply DML: dml=%s, err=%s", dml, err)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("failed to apply DML: dml=%s, err=%s", dml, err)
		}
	}

	tearDown := func() {
		op, err = session.adminClient.UpdateDatabaseDdl(ctx, &adminpb.UpdateDatabaseDdlRequest{
			Database:   dbPath,
			Statements: []string{fmt.Sprintf("DROP TABLE %s", tableId)},
		})
		if err != nil {
			t.Fatalf("failed to drop table: err=%s", err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("failed to drop table: err=%s", err)
		}
	}
	return session, tableId, tearDown
}

func TestSelect(t *testing.T) {
	t.Parallel()
	if skipIntegrateTest {
		t.Skip("Integration tests skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	session, tableId, tearDown := setup(t, ctx, []string{
		"INSERT INTO [[TABLE]] (id, active) VALUES (1, true), (2, false)",
	})
	defer tearDown()

	stmt, err := BuildStatement(fmt.Sprintf("SELECT id, active FROM %s ORDER BY id ASC", tableId))
	if err != nil {
		t.Fatalf("invalid statement: error=%s", err)
	}

	result, err := stmt.Execute(session)
	if err != nil {
		t.Fatalf("unexpected error happened: %s", err)
	}

	opts := []cmp.Option{
		cmpopts.IgnoreFields(Stats{}, "ElapsedTime"),
	}
	expected := &Result{
		ColumnNames: []string{"id", "active"},
		Rows: []Row{
			Row{[]string{"1", "true"}},
			Row{[]string{"2", "false"}},
		},
		Stats: Stats{
			AffectedRows: 2,
		},
		IsMutation: false,
	}

	if !cmp.Equal(result, expected, opts...) {
		t.Errorf("diff: %s", cmp.Diff(result, expected, opts...))
	}
}

func TestDml(t *testing.T) {
	t.Parallel()
	if skipIntegrateTest {
		t.Skip("Integration tests skipped")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	session, tableId, tearDown := setup(t, ctx, []string{})
	defer tearDown()

	stmt, err := BuildStatement(fmt.Sprintf("INSERT INTO %s (id, active) VALUES (1, true), (2, false)", tableId))
	if err != nil {
		t.Fatalf("invalid statement: error=%s", err)
	}

	result, err := stmt.Execute(session)
	if err != nil {
		t.Errorf("unexpected error happened: %s", err)
	}

	opts := []cmp.Option{
		cmpopts.IgnoreFields(Stats{}, "ElapsedTime"),
	}
	expected := &Result{
		ColumnNames: []string{},
		Rows:        []Row{},
		Stats: Stats{
			AffectedRows: 2,
		},
		IsMutation: true,
	}

	if !cmp.Equal(result, expected, opts...) {
		t.Errorf("diff: %s", cmp.Diff(result, expected, opts...))
	}

	// check by query
	query := spanner.NewStatement(fmt.Sprintf("SELECT id, active FROM %s ORDER BY id ASC", tableId))
	iter := session.client.Single().Query(ctx, query)
	defer iter.Stop()
	gotStructs := make([]testTableSchema, 0)
	for {
		row, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			fmt.Errorf("unexpected error: %s", err)
		}
		var got testTableSchema
		if err := row.ToStruct(&got); err != nil {
			fmt.Errorf("unexpected error: %s", err)
		}
		gotStructs = append(gotStructs, got)
	}
	expectedStructs := []testTableSchema{
		{1, true},
		{2, false},
	}
	if !cmp.Equal(gotStructs, expectedStructs) {
		t.Errorf("diff: %s", cmp.Diff(gotStructs, expectedStructs))
	}
}

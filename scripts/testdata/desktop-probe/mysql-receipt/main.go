// Verify a completed Desktop probe in the Work database.
// Usage: go run ./scripts/testdata/desktop-probe/mysql-receipt TASK_ID RUNNER_ID
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// HEX compares the protocol strings byte for byte even with a case-insensitive
// MySQL collation. The two event subqueries share the task's primary key.
const receiptQuery = `
SELECT DATABASE(), t.id, t.runner_id, t.status,
       (SELECT COUNT(*) FROM runner_task_events e WHERE e.task_id = t.id) AS event_count,
       COALESCE(HEX(t.payload ->> '$.result') = HEX('desktop.probe/v1 ok'), 0) AS result_matches,
       (COALESCE(LENGTH(t.lease_owner), 0) = 0 AND t.lease_until IS NULL) AS lease_cleared
FROM runner_tasks t
WHERE t.id = ? AND t.runner_id = ?
  AND HEX(t.provider) = HEX('pinable-desktop')
  AND HEX(t.payload ->> '$.action') = HEX('desktop_probe_v1')
  AND EXISTS (
      SELECT 1 FROM runner_task_events e
      WHERE e.task_id = t.id AND HEX(e.task_id) = HEX(t.id) AND e.seq = 1
        AND HEX(e.type) = HEX('system') AND HEX(e.data) = HEX('desktop.probe/v1 ok')
  )`

type receipt struct {
	Database      string `json:"database"`
	TaskID        string `json:"taskId"`
	RunnerID      string `json:"runnerId"`
	Status        string `json:"status"`
	EventCount    int64  `json:"eventCount"`
	ResultMatches bool   `json:"resultMatches"`
	LeaseCleared  bool   `json:"leaseCleared"`
}

func verify(ctx context.Context, db *sql.DB, taskID, runnerID string) (receipt, error) {
	var r receipt
	err := db.QueryRowContext(ctx, receiptQuery, taskID, runnerID).Scan(
		&r.Database, &r.TaskID, &r.RunnerID, &r.Status, &r.EventCount, &r.ResultMatches, &r.LeaseCleared,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return r, errors.New("no matching Desktop probe and system event for this task and runner")
	case err != nil:
		return r, errors.New("could not read the MySQL probe receipt")
	case r.Database == "":
		return r, errors.New("no active MySQL database")
	case r.TaskID != taskID || r.RunnerID != runnerID:
		return r, errors.New("stored task or runner ID differs from the requested ID")
	case r.Status != "success" || r.EventCount != 1 || !r.ResultMatches || !r.LeaseCleared:
		return r, errors.New("probe receipt failed the success, event count, result or lease check")
	}
	return r, nil
}

func run() error {
	if len(os.Args) != 3 || strings.TrimSpace(os.Args[1]) == "" || strings.TrimSpace(os.Args[2]) == "" {
		return errors.New("usage: mysql-receipt TASK_ID RUNNER_ID")
	}
	dsn := os.Getenv("AGENTFLOW_MYSQL_DSN")
	if strings.TrimSpace(dsn) == "" {
		return errors.New("AGENTFLOW_MYSQL_DSN is required")
	}
	// The driver's own diagnostics can contain connection details.
	if err := mysql.SetLogger(&mysql.NopLogger{}); err != nil {
		return errors.New("could not initialize MySQL logging")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return errors.New("invalid MySQL connection configuration")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		var serverErr *mysql.MySQLError
		if errors.As(err, &serverErr) {
			switch serverErr.Number {
			case 1044, 1045:
				return errors.New("MySQL denied access to the configured account or database")
			case 1049:
				return errors.New("the configured MySQL database does not exist")
			}
		}
		return errors.New("could not connect to MySQL")
	}
	r, err := verify(ctx, db, os.Args[1], os.Args[2])
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(r)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// dbinit: create role wiilink + database wwfc on the local Postgres and apply wfc-server/schema.sql.
// Idempotent. Usage: go run ./dbinit [schema.sql path]
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v4"
)

func main() {
	ctx := context.Background()
	schema := "../../wfc-server/schema.sql"
	if len(os.Args) > 1 {
		schema = os.Args[1]
	}
	admin, err := pgx.Connect(ctx, "postgres://wfc@127.0.0.1:54329/postgres")
	check(err)
	// schema.sql says "OWNER TO wiilink"
	admin.Exec(ctx, "CREATE ROLE wiilink")
	var n int
	check(admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname='wwfc'").Scan(&n))
	if n == 0 {
		_, err = admin.Exec(ctx, "CREATE DATABASE wwfc")
		check(err)
	}
	admin.Close(ctx)

	db, err := pgx.Connect(ctx, "postgres://wfc@127.0.0.1:54329/wwfc")
	check(err)
	var have bool
	check(db.QueryRow(ctx, "SELECT to_regclass('public.users') IS NOT NULL").Scan(&have))
	if !have { // schema.sql is not re-runnable (duplicate PRIMARY KEY), so apply once
		sql, err := os.ReadFile(schema)
		check(err)
		// simple protocol allows multi-statement scripts
		_, err = db.Exec(ctx, string(sql), pgx.QuerySimpleProtocol(true))
		check(err)
	}
	var t int
	check(db.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'").Scan(&t))
	fmt.Println("wwfc ready, public tables:", t)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

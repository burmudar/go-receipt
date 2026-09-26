package db

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

func Default() *sql.DB {
	db, err := Open("app.db")
	if err != nil {
		panic(err)
	}
	return db
}

// Open opens a SQLite database. The caller must close it and apply migrations.
func Open(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// ponytail: serialize SQLite access; use a WAL-backed pool if throughput requires it.
	// A single connection also keeps :memory: databases consistent.
	conn.SetMaxOpenConns(1)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

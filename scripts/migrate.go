package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("initialize database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		log.Fatalf("connect to database: %v", err)
	}

	migrations := []string{
		"migrations/000001_initial_schema.sql",
		"migrations/000002_jobs_audit.sql",
		"migrations/000003_restore_tests_and_resources.sql",
	}

	for _, path := range migrations {
		content, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("read %s: %v", path, err)
		}

		if _, err := db.Exec(ctx, string(content)); err != nil {
			log.Fatalf("apply %s: %v", path, err)
		}

		log.Printf("Applied migration: %s", path)
	}

	log.Println("Local development migrations completed")
}

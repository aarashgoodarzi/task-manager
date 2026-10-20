// Command migrate applies the embedded, versioned schema migrations to the
// configured PostgreSQL database and exits. It is a deployment/development
// step, deliberately separate from the service process: starting the API never
// changes the schema.
package main

import (
	"context"
	"log"
	"time"

	"github.com/aarashgoodarzi/task-manager/internal/infra/config"
	"github.com/aarashgoodarzi/task-manager/internal/infra/database"
)

func main() {
	cfg := config.Load()

	if cfg.Database.DSN == "" {
		log.Fatal("migrate: DATABASE_URL is required")
	}

	db, err := database.New(database.Config{
		DSN:             cfg.Database.DSN,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("migrate: closing database: %v", closeErr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := database.Migrate(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	log.Println("migrate: schema is up to date")
}

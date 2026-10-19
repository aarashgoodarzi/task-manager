// Command task-manager is the process entry point. It performs no logic of
// its own beyond handing control to the composition root.
package main

import (
	"log"

	"github.com/aarashgoodarzi/task-manager/internal/app"
)

func main() {
	application := app.New()

	if err := application.Initialize(); err != nil {
		log.Fatalf("initialize: %v", err)
	}

	if err := application.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
}
